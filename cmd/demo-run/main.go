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

func runBuild(coordinates []string, out chan<- string) {
	pkg := coordinates[1]
	version := coordinates[2]
	artifact := coordinates[3]

	aiClient, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  *project,
		Location: "us-central1",
	})
	if err != nil {
		log.Fatalf("Error creating AI client: %v", err)
	}
	modelName := llm.GeminiPro

	prompt := fmt.Sprintf(`
		Find the source code repository for the package '%s'.
		Just return the URL WITHOUT any additional text.
		Do not include any submodules or subdirectories.
		For example, for the package 'org.apache.camel:camel-support', return 'https://github.com/apache/camel' not 'https://github.com/apache/camel/tree/main/core/camel-support'.
		Use the tools you have at your disposal to find the URL.
		Finally, if you don't find the URL, just return an empty string.`, pkg)
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

	ctx := context.Background()

	count, err := aiClient.Models.CountTokens(ctx, modelName, contents, &genai.CountTokensConfig{
		Tools: []*genai.Tool{
			{GoogleSearch: &genai.GoogleSearch{}},
		},
	})
	if err != nil {
		log.Fatalf("Error counting tokens: %v", err)
	}
	if count.TotalTokens > 32_000 {
		out <- fmt.Sprintf("%s,ERROR: prompt too long (%d tokens)", pkg, count.TotalTokens)
		return
	}

	resp, err := aiClient.Models.GenerateContent(ctx, modelName, contents, config)
	if err != nil {
		out <- fmt.Sprintf("%s,ERROR: generating content: %v", pkg, err)
		return
	}

	repoURL := strings.TrimSpace(resp.Text())

	inferenceOutputBufferStdout := &bytes.Buffer{}
	inferenceOutputBufferStderr := &bytes.Buffer{}

	directory := path.Join(*localStore, "inference-logs", pkg, version, artifact)
	os.MkdirAll(directory, 0755)
	inferenceLog, err := os.Create(path.Join(directory, "logs"))
	if err != nil {
		log.Fatal(errors.Wrap(err, "creating inference log"))
	}
	defer inferenceLog.Close()

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--memory", "10g", "ctl", "infer",
		"--ecosystem", "maven",
		"--package", pkg,
		"--version", version,
		"--artifact", artifact,
		"--repo-hint", repoURL)
	cmd.Stdout = inferenceOutputBufferStdout
	cmd.Stderr = inferenceOutputBufferStderr

	err = cmd.Run()

	inferenceLog.WriteString(fmt.Sprintf("%sSTDOUT:\n%s\n\nSTDERR:\n%s\n%v\n", cmd.String(), inferenceOutputBufferStdout.String(), inferenceOutputBufferStderr.String(), err))

	if err != nil {
		out <- fmt.Sprintf("%s,ERROR: inference: %v\n", pkg, err)
		return
	}

	var schemaStrategy schema.StrategyOneOf
	json.NewDecoder(inferenceOutputBufferStdout).Decode(&schemaStrategy)

	s, err := schemaStrategy.Strategy()
	if err != nil {
		log.Fatal(errors.Wrap(err, "parsing strategy"))
	}

	inp := rebuild.Input{Target: rebuild.Target{
		Ecosystem: rebuild.Maven,
		Package:   pkg,
		Version:   version,
		Artifact:  artifact,
	}, Strategy: s}
	if err != nil {
		log.Fatal(errors.Wrap(err, "generating plan"))
	}

	localDockerExecutor, err := local.NewDockerBuildExecutor(local.DockerBuildExecutorConfig{
		MaxParallel:     6,
		RetainContainer: false,
		MaxMemory:       "10g",
		TempDirBase:     path.Join(*localStore, "artifacts"),
	})
	if err != nil {
		log.Fatal(errors.Wrap(err, "creating docker executor"))
	}

	buildHandle, err := localDockerExecutor.Start(ctx, inp, build.Options{
		BuildID: strings.ReplaceAll(pkg, ":", "_") + "_" + version,
		Resources: build.Resources{
			AssetStore:      rebuild.NewFilesystemAssetStore(osfs.New(*localStore)),
			BaseImageConfig: build.DefaultBaseImageConfig(),
		},
	})
	if err != nil {
		log.Fatal(errors.Wrap(err, "starting build"))
	}

	result, err := buildHandle.Wait(ctx)
	out <- fmt.Sprintf("%s,%s", pkg, "Build Succeeded")
	if err != nil {
		log.Printf("error waiting for build: %v", err)
	}
	if result.Error != nil {
		log.Printf("build failed: %v", result.Error)
	}
}

func main() {
	// Read CSV from stdin
	flag.Parse()

	reader := csv.NewReader(os.Stdin)
	records, err := reader.ReadAll()
	if err != nil {
		panic(err)
	}

	directory := path.Join(*localStore)
	os.MkdirAll(directory, 0755)

	p := pipe.ParInto(6, pipe.FromSlice(records), runBuild)

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
