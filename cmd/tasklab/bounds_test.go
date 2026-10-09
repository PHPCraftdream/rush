package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
)

type cliTestSpaces struct{}

func (cliTestSpaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestCLIInputRejectionsPreserveCheckpoint(t *testing.T) {
	checkpoint := cliTestSaveCheckpoint(t, "Retained")
	for _, kind := range []string{"scenario", "checkpoint"} {
		var value any = cliTestScenario(cliTestStep("read", "read", `{"op":"view","id":"n0"}`))
		if kind == "checkpoint" {
			value = checkpoint
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, invalid := range []string{"oversize", "malformed", "duplicate"} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				dir := t.TempDir()
				destination := filepath.Join(dir, "retained.json")
				if err := saveCheckpoint(destination, checkpoint); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(destination)
				if err != nil {
					t.Fatal(err)
				}
				inputPath := filepath.Join(dir, "invalid.json")
				var source io.Reader
				switch invalid {
				case "oversize":
					source = io.MultiReader(bytes.NewReader(data), io.LimitReader(cliTestSpaces{}, int64(lab.MaxInputBytes-len(data)+1)))
				case "malformed":
					source = strings.NewReader(`{"schema_version":`)
				case "duplicate":
					duplicate := strings.Replace(string(data), `"schema_version":1`, `"schema_version":1,"\u0073chema_version":1`, 1)
					if duplicate == string(data) {
						t.Fatal("duplicate fixture did not change input")
					}
					source = strings.NewReader(duplicate)
				}
				file, err := os.Create(inputPath)
				if err != nil {
					t.Fatal(err)
				}
				_, copyErr := io.Copy(file, source)
				closeErr := file.Close()
				if copyErr != nil || closeErr != nil {
					t.Fatalf("fixture write=%v close=%v", copyErr, closeErr)
				}
				args := []string{"run", "--load", destination, "--snapshot", destination, inputPath}
				if kind == "checkpoint" {
					scenarioPath := filepath.Join(dir, "scenario.json")
					cliTestJSONFile(t, scenarioPath, cliTestScenario())
					args = []string{"run", "--load", inputPath, "--snapshot", destination, scenarioPath}
				}
				var output, diagnostics bytes.Buffer
				if code := runCLI(args, strings.NewReader(""), &output, &diagnostics); code != 1 || output.Len() != 0 || diagnostics.Len() == 0 {
					t.Fatalf("exit=%d output length=%d diagnostics length=%d", code, output.Len(), diagnostics.Len())
				}
				cliTestSavePreserved(t, destination, original)
				if kind == "checkpoint" {
					output.Reset()
					diagnostics.Reset()
					if code := runCLI([]string{"inspect", inputPath}, strings.NewReader(""), &output, &diagnostics); code != 1 || output.Len() != 0 || diagnostics.Len() == 0 {
						t.Fatalf("inspect exit=%d output length=%d diagnostics length=%d", code, output.Len(), diagnostics.Len())
					}
					cliTestSavePreserved(t, destination, original)
				}
			})
		}
	}
}
