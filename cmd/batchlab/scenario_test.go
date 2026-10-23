package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch/lab"
)

func TestMainEveryScenarioFixture(t *testing.T) {
	// L3/F/V/R/C/W mutation: a CLI that ignores declared exits or bypasses ingress fails a fixture.
	paths, err := filepath.Glob("../../internal/jobbatch/lab/testdata/scenarios/*")
	if err != nil || len(paths) == 0 {
		t.Fatal("fixtures", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var declared lab.Scenario
			if err = json.Unmarshal(data, &declared); err != nil {
				t.Fatal(err)
			}
			if declared.Expect == nil || declared.Expect.ExitCode == nil {
				t.Fatal("missing declared exit")
			}
			name := strings.TrimSuffix(filepath.Base(path), ".json")
			invalid := strings.HasPrefix(name, "v-a-")
			_, loadErr := lab.LoadScenario(bytes.NewReader(data))
			if (loadErr != nil) != invalid {
				t.Fatalf("fixture validity: %s", lab.ScenarioValidationRule(loadErr))
			}
			want := *declared.Expect.ExitCode
			if invalid {
				want = lab.ExitInvalid
			}
			var out, diag bytes.Buffer
			if got := Main(context.Background(), []string{"run", path}, &out, &diag); got != want {
				t.Fatalf("exit=%d want=%d diagnostics=%s", got, want, diag.String())
			}
		})
	}
}
