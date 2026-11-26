package verify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestTrackedCheckoutResultsAreFourGuardOnly(t *testing.T) {
	root := filepath.Clean(".")
	env, code, err := Run(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !env.OK || len(env.Stages) != 1 || len(env.Stages[0].Guards) != 4 {
		t.Fatalf("exit=%d envelope=%+v", code, env)
	}
	for _, g := range env.Stages[0].Guards {
		if g.Status != "pass" || g.ExitCode != 0 || g.Violations != 0 || g.Summary == "" {
			t.Errorf("%s did not produce a meaningful passing result: %+v", g.Name, g)
		}
		if (g.Name == "dir-entries" || g.Name == "dir-files") && (g.Directories == 0 || g.Files == 0) {
			t.Errorf("%s examined no directories or files: %+v", g.Name, g)
		}
		if (g.Name == "file-size" || g.Name == "file-size-900") && g.Files == 0 {
			t.Errorf("%s examined no files: %+v", g.Name, g)
		}
		if g.DurationMS < 0 {
			t.Errorf("negative duration: %+v", g)
		}
	}

	data, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
}
