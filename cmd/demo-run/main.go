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

	"github.com/google/oss-rebuild/internal/llm"
	"github.com/google/oss-rebuild/tools/ctl/pipe"
	"github.com/pkg/errors"
	"google.golang.org/genai"
)

var localStore = flag.String("localStore", "", "The base directory for logs.")
var project = flag.String("project", "", "GCP project ID.")

type llmResponse struct {
	RepoURL   string `json:"repoURL"`
	Reasoning string `json:"reasoning"`
}

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

	prompt := fmt.Sprintf(`Your task is to find the source code repository for a given Maven package "%s".
Analyze the package identifier and use the tools available to you to find the canonical git repository URL.

Return your answer ONLY as a single JSON object with the following structure:
{
  "repoURL": "string | null",
  "reasoning": "string"
}`, pkg)
	contents := []*genai.Content{
		{
			Parts: []*genai.Part{
				{Text: prompt},
			},
			Role: genai.RoleUser,
		},
	}
	config := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"repoURL": {
					Type: genai.TypeString,
				},
				"reasoning": {
					Type: genai.TypeString,
				},
			},
			Required: []string{"repoURL", "reasoning"},
		},
		Temperature: genai.Ptr(float32(0.0)),
		// Tools: []*genai.Tool{
		// 	{GoogleSearch: &genai.GoogleSearch{}},
		// },
	}

	ctx := context.Background()

	count, err := aiClient.Models.CountTokens(ctx, modelName, contents, &genai.CountTokensConfig{
		// Tools: []*genai.Tool{
		// 	{GoogleSearch: &genai.GoogleSearch{}},
		// },
	})
	if err != nil {
		out <- fmt.Sprintf("%s,ERROR: counting tokens: %v,", pkg, err)
		return
	}
	if count.TotalTokens > 32_000 {
		out <- fmt.Sprintf("%s,ERROR: prompt too long (%d tokens)", pkg, count.TotalTokens)
		return
	}

	resp, err := aiClient.Models.GenerateContent(ctx, modelName, contents, config)
	if err != nil {
		out <- fmt.Sprintf("%s,ERROR: generating content: %v,", pkg, err)
		return
	}
	var decodedResp llmResponse
	if err := json.Unmarshal([]byte(resp.Text()), &decodedResp); err != nil {
		out <- fmt.Sprintf("%s,ERROR: decoding response: %v,", pkg, err)
		return
	}

	repoURL := strings.TrimSpace(decodedResp.RepoURL)

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

	inferenceLog.WriteString(fmt.Sprintf("%s\nSTDOUT:\n%s\n\nSTDERR:\n%s\n%v\n", cmd.String(), inferenceOutputBufferStdout.String(), inferenceOutputBufferStderr.String(), err))

	if err != nil {
		out <- fmt.Sprintf("%s,ERROR: %v,%s", pkg, err, decodedResp.Reasoning)
		return
	}
	out <- fmt.Sprintf("%s,%s,%s", pkg, repoURL, decodedResp.Reasoning)
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
