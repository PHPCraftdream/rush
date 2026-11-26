package verify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	static "github.com/PHPCraftdream/rush/internal/serve/ops/verify/static"
)

func TestTrackedCheckoutResultsAreSevenGuardOnly(t *testing.T) {
	root := filepath.Clean(".")
	env, code, err := Run(context.Background(), root, false)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !env.OK || len(env.Stages) != 1 || len(env.Stages[0].Guards) != 7 {
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

// TestPkgNamesRealCheckoutSeededAllowlistSize mirrors
// .githooks/check_pkg_names_selftest.sh:215-219 (case 8): the guard's
// summary on THIS checkout must report the seeded allowlist size --
// currently 3 entries in .githooks/pkg_names_allowlist.txt (hyper 2,
// lab 2, session 2).
func TestPkgNamesRealCheckoutSeededAllowlistSize(t *testing.T) {
	root := filepath.Clean(".")
	env, code, err := Run(context.Background(), root, false)
	if err != nil || code != 0 {
		t.Fatalf("exit=%d err=%v", code, err)
	}
	for _, g := range env.Stages[0].Guards {
		if g.Name == "pkg-names" {
			if !strings.Contains(g.Summary, "3 allowlisted collision(s)") {
				t.Fatalf("summary must report the seeded allowlist size: %q", g.Summary)
			}
			return
		}
	}
	t.Fatal("pkg-names guard missing")
}

func TestSummarizeGuardsBrokenInputDominatesExitCode(t *testing.T) {
	ok := func(name string) static.Result {
		return static.Result{Name: name, Status: "pass", Directories: 1, Files: 1}
	}
	with := func(r static.Result, others ...string) []static.Result {
		out := []static.Result{r}
		for _, n := range others {
			out = append(out, ok(n))
		}
		return out
	}
	broken := static.Result{Name: "db-release-pairing", Status: "error", Directories: 1, Files: 1, Excerpt: "git grep failed"}
	failing := static.Result{Name: "pkg-names", Status: "fail", Directories: 1, Files: 1, Violations: 1}
	cases := []struct {
		name    string
		results []static.Result
		want    int
	}{
		{"all pass", with(ok("dir-entries"), "dir-files", "file-size", "file-size-900", "layers", "pkg-names", "db-release-pairing"), 0},
		{"a violation alone is exit 1", with(failing, "dir-entries", "dir-files", "file-size", "file-size-900", "layers", "db-release-pairing"), 1},
		{"broken input alone is exit 3, never 0", with(broken, "dir-entries", "dir-files", "file-size", "file-size-900", "layers", "pkg-names"), 3},
		{"broken input beats a violation", append(with(broken, "dir-entries", "dir-files", "file-size", "file-size-900", "layers"), failing), 3},
		{"a violation after broken input does not downgrade it", append(with(failing, "dir-entries"), broken), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guards, code := summarizeGuards(tc.results)
			if code != tc.want {
				t.Fatalf("aggregate exit code = %d, want %d (guards=%+v)", code, tc.want, guards)
			}
			if len(guards) != len(tc.results) {
				t.Fatalf("every guard must be reported: %d of %d", len(guards), len(tc.results))
			}
		})
	}
}
