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
	"regexp"
	"strings"

	"github.com/google/oss-rebuild/internal/llm"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/google/oss-rebuild/pkg/rebuild/schema"
	"github.com/google/oss-rebuild/tools/ctl/pipe"
	"github.com/pkg/errors"
	"google.golang.org/genai"
)

var localStore = flag.String("localStore", "", "The base directory for logs.")
var project = flag.String("project", "", "GCP project ID.")
var concurrency = flag.Int("concurrency", 2, "Number of concurrent builds.")
var maxAttempts = flag.Int("max-attempts", 2, "Maximum number of build attempts per package.")

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

// promptForRepoHint generates a prompt to find a repo URL based on inference failure logs.
func promptForRepoHint(pkg, inferenceLog string) string {
	prompt := fmt.Sprintf(
		"Based on the following logs for package '%s', find the correct source code repository URL and provide an explanation of the reasoning behind your choice. Be brief.\n", pkg)

	if inferenceLog != "" {
		prompt += fmt.Sprintf("\nInference Logs:\n%s\n", inferenceLog)
	}

	prompt += `
		The logs can have the following errors:
		- "no git ref": This means the inference process could not find any commit or tag in the repository that matches the package version.
		- "no valid git ref": This means the inference process found a commit or tag, but it did not correspond to the expected package version.
		- "cloning repo":
			- This can be either the repository is deleted or private
			- the URL is a webpage
			- it is a gitee repo
			- SSH access is required. Usually the SSH is required for submodules.
		- "unsupported repo type": This means the URL does not point to a git repository.
		In all cases, the URL is likely incorrect or incomplete. Your task is to find the correct URL.
		`

	prompt += `
        In your response, include the final URL on a new line, prefixed with "URL: ".
        Do not include any submodules or subdirectories in the URL.
        For example:
        This package is part of the Apache Camel project, which is hosted on GitHub.
        URL: https://github.com/apache/camel

        If you cannot determine the URL, return an empty response.`

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

// parseURLFromAIResponse extracts the reasoning and the URL from the AI's text response using regex.
func parseURLFromAIResponse(responseText string) (string, string) {
	// Regex to find a URL prefixed with "URL: " and capture the URL.
	re := regexp.MustCompile(`URL:\s*(https?://[^\s]+)`)
	matches := re.FindStringSubmatch(responseText)

	var url string
	if len(matches) > 1 {
		// The first submatch (index 1) is the captured URL.
		url = matches[1]
	}

	return responseText, url
}

// getRepoHint calls the AI model to find a repo URL based on failure logs.
func getRepoHint(ctx context.Context, aiClient *genai.Client, modelName, pkg, inferenceLog string) (string, error) {
	prompt := promptForRepoHint(pkg, inferenceLog)
	return callAI(ctx, aiClient, modelName, prompt)
}

// runInferenceWithRetries orchestrates the iterative inference process.
func runInferenceWithRetries(coordinates []string, out chan<- string) {
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

	var repoURL string = ""
	var infLogs string = ""
	var strategy rebuild.Strategy = nil
	var infErr error

	// --- INFERENCE LOOP (Attempts 1 to maxAttempts) ---
	for attempt := 1; attempt <= *maxAttempts; attempt++ {
		var logMsg string
		// Attempt 1 is Manual
		if attempt == 1 {
			logMsg = fmt.Sprintf("[%s] Starting attempt 1/%d (Manual)", pkg, *maxAttempts)
		} else {
			// Attempts 2+ are AI-assisted
			log.Printf("[%s] Getting AI hint (with inference logs) for attempt %d...", pkg, attempt)
			fullAIResponse, err := getRepoHint(ctx, aiClient, modelName, pkg, infLogs)
			if err != nil {
				out <- fmt.Sprintf("%s,ERROR: AI hint failed for attempt %d: %v", pkg, attempt, err)
				return
			}

			reasoning, hint := parseURLFromAIResponse(fullAIResponse)
			log.Printf("[%s] AI Reasoning: %s", pkg, reasoning)

			// Log reasoning to its own file
			if reasoning != "" {
				reasoningLogPath := path.Join(logDir, fmt.Sprintf("reasoning_%d.txt", attempt))
				if err := os.WriteFile(reasoningLogPath, []byte(reasoning), 0644); err != nil {
					log.Printf("[%s] WARNING: Failed to write reasoning log for attempt %d: %v", pkg, attempt, err)
				}
			}

			if hint == "" {
				out <- fmt.Sprintf("%s,ERROR: AI found no hint for attempt %d. Full response: %s", pkg, attempt, fullAIResponse)
				return
			}
			repoURL = hint
			logMsg = fmt.Sprintf("[%s] Starting attempt %d/%d (AI-Assisted, RepoHint: '%s')", pkg, attempt, *maxAttempts, repoURL)
		}

		// Run Inference
		log.Println(logMsg)
		infLogFile := path.Join(logDir, fmt.Sprintf("inference_%d_log.txt", attempt))
		strategy, infLogs, infErr = runInference(ctx, pkg, version, artifact, repoURL, infLogFile)

		// Check success
		if infErr == nil && strategy != nil {
			log.Printf("[%s] Inference succeeded on attempt %d.", pkg, attempt)
			out <- fmt.Sprintf("%s,Inference Succeeded (attempt %d)", pkg, attempt)
			return // Exit on success
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

	if *maxAttempts < 1 {
		out <- fmt.Sprintf("%s,ERROR: Invalid maxAttempts (%d)", pkg, *maxAttempts)
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

	p := pipe.ParInto(*concurrency, pipe.FromSlice(records), runInferenceWithRetries)

	summaryCsvPath := path.Join(*localStore, "summary.csv")
	summaryCsvFile, err := os.Create(summaryCsvPath)
	if err != nil {
		log.Fatal(errors.Wrap(err, "creating summary CSV"))
	}
	defer summaryCsvFile.Close()
	for msg := range p.Out() {
		log.Println(msg)
		_, err := summaryCsvFile.WriteString(strings.TrimSpace(msg) + "\n")
		if err != nil {
			log.Fatal(errors.Wrap(err, "writing to summary CSV"))
		}
	}
}
