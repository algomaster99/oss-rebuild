package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/google/oss-rebuild/internal/llm"
	"github.com/google/oss-rebuild/pkg/build"
	"github.com/google/oss-rebuild/pkg/build/local"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/google/oss-rebuild/pkg/rebuild/schema"
	"github.com/google/oss-rebuild/tools/ctl/pipe"
	"github.com/pkg/errors"
	"google.golang.org/genai"
)

var localStore = flag.String("localStore", "", "The base directory for logs.")
var project = flag.String("project", "", "GCP project ID.")
var concurrency = flag.Int("concurrency", 2, "Number of concurrent builds.")
var maxAttempts = flag.Int("maxAttempts", 2, "Maximum number of build attempts per package.")

// runInference executes the 'ctl infer' command.
// It logs stdout/stderr to logFilepath and returns the parsed strategy,
// a combined log string for AI analysis, and an error.
func runInference(ctx context.Context, pkg, version, artifact, repoURL, logFilepath string) (rebuild.Strategy, string, error) {
	inferenceOutputBufferStdout := &bytes.Buffer{}
	inferenceOutputBufferStderr := &bytes.Buffer{}

	inferenceLog, err := os.Create(logFilepath)
	if err != nil {
		return nil, "", errors.Wrap(err, "creating inference log")
	}
	defer inferenceLog.Close()

	cmdArgs := []string{
		"run", "--rm", "--memory", "10g", "ctl", "infer",
		"--ecosystem", "maven",
		"--package", pkg,
		"--version", version,
		"--artifact", artifact,
	}

	if repoURL != "" {
		cmdArgs = append(cmdArgs, "--repo-hint", repoURL)
	}

	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	cmd.Stdout = inferenceOutputBufferStdout
	cmd.Stderr = inferenceOutputBufferStderr

	err = cmd.Run()

	// Write logs regardless of error
	stdoutStr := inferenceOutputBufferStdout.String()
	stderrStr := inferenceOutputBufferStderr.String()
	combinedLogs := fmt.Sprintf("STDOUT:\n%s\n\nSTDERR:\n%s\n", stdoutStr, stderrStr)

	logEntry := fmt.Sprintf("%s\n%s\n%v\n", cmd.String(), combinedLogs, err)
	inferenceLog.WriteString(logEntry)

	if err != nil {
		// Return the logs even on failure, so the AI can analyze them
		return nil, combinedLogs, errors.Wrapf(err, "inference command failed: %s", stderrStr)
	}

	var schemaStrategy schema.StrategyOneOf
	if err := json.NewDecoder(inferenceOutputBufferStdout).Decode(&schemaStrategy); err != nil {
		return nil, combinedLogs, errors.Wrap(err, "decoding inference strategy")
	}

	s, err := schemaStrategy.Strategy()
	if err != nil {
		return nil, combinedLogs, errors.Wrap(err, "parsing strategy")
	}

	return s, combinedLogs, nil
}

// runBuildInternal handles the actual build execution and waits for the result.
func runBuildInternal(ctx context.Context, executor *local.DockerBuildExecutor, inp rebuild.Input, opts build.Options) (build.Result, error) {
	buildHandle, err := executor.Start(ctx, inp, opts)
	if err != nil {
		return build.Result{}, errors.Wrap(err, "starting build")
	}
	// Wait(ctx) can return its own error (e.g., context canceled)
	// and result.Error contains errors from within the build process.
	result, waitErr := buildHandle.Wait(ctx)
	return result, waitErr
}

// promptForRepoHint generates a prompt to find a repo URL based on failure logs.
func promptForRepoHint(pkg, inferenceLog string) string {
	prompt := fmt.Sprintf(
		"Based on the following logs for package '%s', find the correct source code repository URL.\n", pkg)

	if inferenceLog != "" {
		prompt += fmt.Sprintf("\nInference Logs:\n%s\n", inferenceLog)
	}

	prompt += `
        Just return the URL WITHOUT any additional text.
        Do not include any submodules or subdirectories.
        For example, for the package 'org.apache.camel:camel-support', return 'https://github.com/apache/camel' not 'https://github.com/apache/camel/tree/main/core/camel-support'.
        Use the tools you have at your disposal to find the URL.
        Finally, if you don't find the URL, just return an empty string.`

	return prompt
}

// callAI executes a given prompt against the genai model.
func callAI(ctx context.Context, aiClient *genai.Client, modelName, prompt string) (string, error) {
	contents := []*genai.Content{
		{
			Parts: []*genai.Part{
				{Text: prompt},
			},
			Role: genai.RoleUser,
		},
	}
	config := &genai.GenerateContentConfig{
		Temperature: genai.Ptr(float32(0.0)),
		Tools: []*genai.Tool{
			{GoogleSearch: &genai.GoogleSearch{}},
		},
	}

	count, err := aiClient.Models.CountTokens(ctx, modelName, contents, &genai.CountTokensConfig{
		Tools: []*genai.Tool{
			{GoogleSearch: &genai.GoogleSearch{}},
		},
	})
	if err != nil {
		return "", errors.Wrap(err, "counting tokens")
	}
	if count.TotalTokens > 64_000 {
		return "", fmt.Errorf("prompt too long (%d tokens)", count.TotalTokens)
	}

	resp, err := aiClient.Models.GenerateContent(ctx, modelName, contents, config)
	if err != nil {
		return "", errors.Wrap(err, "generating content")
	}

	return strings.TrimSpace(resp.Text()), nil
}

// getRepoHint calls the AI model to find a repo URL based on failure logs.
func getRepoHint(ctx context.Context, aiClient *genai.Client, modelName, pkg, inferenceLog string) (string, error) {
	prompt := promptForRepoHint(pkg, inferenceLog)
	return callAI(ctx, aiClient, modelName, prompt)
}

// runBuild orchestrates the iterative build process.
func runBuild(coordinates []string, out chan<- string) {
	ctx := context.Background()
	pkg := coordinates[1]
	version := coordinates[2]
	artifact := coordinates[3]

	// Setup AI client
	aiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  *project,
		Location: "us-central1",
	})
	if err != nil {
		log.Printf("Error creating AI client: %v", err)
		out <- fmt.Sprintf("%s,ERROR: creating AI client: %v", pkg, err)
		return
	}
	modelName := llm.GeminiPro

	// Setup log directory
	logDir := path.Join(*localStore, "inference-logs", pkg, version, artifact)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("Error creating log directory: %v", err)
		out <- fmt.Sprintf("%s,ERROR: creating log directory: %v", pkg, err)
		return
	}

	// Setup builder executor
	localDockerExecutor, err := local.NewDockerBuildExecutor(local.DockerBuildExecutorConfig{
		MaxParallel:     *concurrency,
		RetainContainer: false,
		MaxMemory:       "10g",
		TempDirBase:     path.Join(*localStore, "artifacts"),
	})
	if err != nil {
		log.Printf("Error creating docker executor: %v", err)
		out <- fmt.Sprintf("%s,ERROR: creating docker executor: %v", pkg, err)
		return
	}

	var repoURL string = ""
	var infLogs string = ""
	var strategy rebuild.Strategy = nil
	var infErr error

	// --- INFERENCE LOOP (Attempts 1 to maxAttempts) ---
	for attempt := 1; attempt <= *maxAttempts; attempt++ {
		if attempt > 1 {
			// Attempts 2+ are AI-assisted
			log.Printf("[%s] Getting AI hint (with inference logs) for attempt %d...", pkg, attempt)
			hint, err := getRepoHint(ctx, aiClient, modelName, pkg, infLogs)
			if err != nil {
				out <- fmt.Sprintf("%s,ERROR: AI hint failed for attempt %d: %v", pkg, attempt, err)
				return
			}
			if hint == "" {
				out <- fmt.Sprintf("%s,ERROR: AI found no hint for attempt %d", pkg, attempt)
				return
			}
			repoURL = hint
			log.Printf("[%s] Got AI hint for attempt %d: %s", pkg, attempt, repoURL)
		} else {
			// Attempt 1 is Manual
			log.Printf("[%s] Starting attempt 1/%d (Manual)", pkg, *maxAttempts)
		}

		// Run Inference
		log.Printf("[%s] Starting inference attempt %d/%d (RepoHint: '%s')", pkg, attempt, *maxAttempts, repoURL)
		infLogFile := path.Join(logDir, fmt.Sprintf("inference_%d_log.txt", attempt))
		strategy, infLogs, infErr = runInference(ctx, pkg, version, artifact, repoURL, infLogFile)

		// Check success
		if infErr == nil && strategy != nil {
			log.Printf("[%s] Inference succeeded on attempt %d.", pkg, attempt)
			break // Exit loop on success
		}

		// Handle failure
		log.Printf("[%s] Inference failed on attempt %d.", pkg, attempt)
		if attempt == *maxAttempts {
			// This was the last attempt
			out <- fmt.Sprintf("%s,ERROR: Inference failed on final attempt %d: %v", pkg, attempt, infErr)
			return
		}
		// Otherwise, loop continues to next (AI-assisted) attempt
	}

	// --- BUILD PHASE (Runs exactly once, only if inference succeeded) ---
	if strategy == nil {
		// This should be unreachable if maxAttempts >= 1, but guards against loop failure
		log.Printf("[%s] No valid strategy found after all attempts.", pkg)
		out <- fmt.Sprintf("%s,ERROR: Inference failed on all %d attempts.", pkg, *maxAttempts)
		return
	}

	log.Printf("[%s] Proceeding to build.", pkg)
	inp := rebuild.Input{Target: rebuild.Target{
		Ecosystem: rebuild.Maven,
		Package:   pkg,
		Version:   version,
		Artifact:  artifact,
	}, Strategy: strategy}

	buildOpts := build.Options{
		BuildID: fmt.Sprintf("%s_%s_build", strings.ReplaceAll(pkg, ":", "_"), version),
		Resources: build.Resources{
			AssetStore:      rebuild.NewFilesystemAssetStore(osfs.New(*localStore)),
			BaseImageConfig: build.DefaultBaseImageConfig(),
		},
	}

	result, buildErr := runBuildInternal(ctx, localDockerExecutor, inp, buildOpts)

	// Report final build result
	if buildErr == nil && result.Error == nil {
		out <- fmt.Sprintf("%s,Build Succeeded", pkg)
		log.Printf("[%s] Build Succeeded", pkg)
	} else {
		var buildLog string
		if buildErr != nil {
			buildLog += fmt.Sprintf("Build Wait Error: %v\n", buildErr)
		}
		if result.Error != nil {
			buildLog += fmt.Sprintf("Build Result Error: %v\n", result.Error)
		}
		out <- fmt.Sprintf("%s,ERROR: Build Failed: %s", pkg, buildLog)
		log.Printf("[%s] Build Failed: %s", pkg, buildLog)
	}
}

func main() {
	// Read CSV from stdin
	flag.Parse()

	if *localStore == "" || *project == "" {
		log.Fatal("Both --localStore and --project flags are required.")
	}

	reader := csv.NewReader(os.Stdin)
	records, err := reader.ReadAll()
	if err != nil {
		panic(err)
	}

	directory := path.Join(*localStore)
	os.MkdirAll(directory, 0755)

	p := pipe.ParInto(*concurrency, pipe.FromSlice(records), runBuild)

	summaryCsvPath := path.Join(*localStore, "summary.csv")
	summaryCsvFile, err := os.Create(summaryCsvPath)
	if err != nil {
		log.Fatal(errors.Wrap(err, "creating summary CSV"))
	}
	defer summaryCsvFile.Close()
	for msg := range p.Out() {
		log.Println(msg) // Also log to stdout/stderr
		_, err := summaryCsvFile.WriteString(strings.TrimSpace(msg) + "\n")
		if err != nil {
			log.Fatal(errors.Wrap(err, "writing to summary CSV"))
		}
	}
}
