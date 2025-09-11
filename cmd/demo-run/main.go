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
	"github.com/google/oss-rebuild/pkg/build"
	"github.com/google/oss-rebuild/pkg/build/local"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/google/oss-rebuild/pkg/rebuild/schema"
	"github.com/google/oss-rebuild/tools/ctl/pipe"
	"github.com/pkg/errors"
)

var localStore = flag.String("localStore", "", "The base directory for logs.")

func runBuild(coordinates []string, out chan<- string) {
	pkg := coordinates[1]
	version := coordinates[2]
	artifact := coordinates[3]

	ctx := context.Background()

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
		"--artifact", artifact)
	cmd.Stdout = inferenceOutputBufferStdout
	cmd.Stderr = inferenceOutputBufferStderr

	err = cmd.Run()

	inferenceLog.WriteString(fmt.Sprintf("%sSTDOUT:\n%s\n\nSTDERR:\n%s\n%v\n", cmd.String(), inferenceOutputBufferStdout.String(), inferenceOutputBufferStderr.String(), err))

	if err != nil {
		out <- fmt.Sprintf("Inference failure for %s:%s:%s: %v\n", pkg, version, artifact, err)
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
	out <- "Done with " + pkg + "\n"
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

	p := pipe.ParInto(6, pipe.FromSlice(records), runBuild)

	for msg := range p.Out() {
		log.Print(msg)
	}
}
