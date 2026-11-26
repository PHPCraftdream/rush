package verify

// Command-level port of .githooks/check_layers_selftest.sh cases 1-11
// (case 12, the real repository, is TestTrackedCheckoutResultsAreSevenGuardOnly).
// Every case runs the real `rush verify guards --json` code path over a
// throwaway git repository, so verdicts, envelope structure and exit
// codes are asserted through the production code, not a mirror.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	static "github.com/PHPCraftdream/rush/internal/serve/ops/verify/static"
)

const layerChainText = "kit < store < session < config < agent < app < serve"

// layerFixture builds a throwaway git repository (with go.mod "module
// fixture" and empty ratchet lists from initFixture) plus extra list
// contents and Go files; it returns the root path. Package names are
// kept unique across directories so the pkg-names guard stays silent.
func layerFixture(t *testing.T, lists map[string]string, files map[string]string) string {
	t.Helper()
	body := map[string][]byte{}
	for p, c := range files {
		body[p] = []byte(c)
	}
	for name, content := range lists {
		body[".githooks/"+name] = []byte(content)
	}
	return initFixture(t, body, nil)
}

// pkgFile renders a single-package .go file, optionally with imports.
func pkgFile(pkg string, imports ...string) string {
	s := "package " + pkg
	for _, imp := range imports {
		s += "\n\nimport \"" + imp + "\""
	}
	return s + "\n"
}

func layerGuard(t *testing.T, root string) (Envelope, Guard) {
	t.Helper()
	exit, out, err := runCobra(t, root, "--json")
	var env Envelope
	if e := json.Unmarshal(out, &env); e != nil {
		t.Fatalf("invalid JSON (exit=%d err=%v): %v", exit, err, e)
	}
	for _, g := range env.Stages[0].Guards {
		if g.Name == "layers" {
			return env, g
		}
	}
	t.Fatalf("layers guard missing from envelope: %+v", env.Stages[0])
	return env, Guard{}
}

func TestLayersSelftestCases(t *testing.T) {
	cases := []struct {
		name    string
		lists   map[string]string
		files   map[string]string
		exit    int
		excerpt string
	}{
		{
			name: "clean module passes; downward and non-internal importers exempt",
			files: map[string]string{
				"internal/kit/a/pkg.go":   pkgFile("a"),
				"internal/agent/b/pkg.go": pkgFile("b", "fixture/internal/kit/a"),
				"internal/app/d/pkg.go":   pkgFile("d"),
				"internal/serve/pkg.go":   pkgFile("serve"),
				"internal/serve/c/pkg.go": pkgFile("c"),
				"main.go":                 pkgFile("main", "fixture/internal/serve") + "\nvar _ = serve.Pkg\n",
			},
			exit: 0,
		},
		{
			name: "kit importing agent fails (upward production import)",
			files: map[string]string{
				"internal/agent/b/pkg.go": pkgFile("b"),
				"internal/kit/a/pkg.go":   pkgFile("a", "fixture/internal/agent/b"),
			},
			exit: 1,
			// The shell's deterministic order prints the "fix the import" hint first for an unrecorded violator.
			excerpt: "fix the import, or record the CURRENT count in layer_exceptions.txt as \"fixture/internal/kit/a 1\"",
		},
		{
			name: "same-group import passes",
			files: map[string]string{
				"internal/kit/a/pkg.go":  pkgFile("a"),
				"internal/kit/a2/pkg.go": pkgFile("a2", "fixture/internal/kit/a"),
			},
			exit: 0,
		},
		{
			name: "internal/app/itest may import anything",
			files: map[string]string{
				"internal/serve/c/pkg.go":   pkgFile("c"),
				"internal/app/itest/pkg.go": pkgFile("itest", "fixture/internal/serve/c"),
			},
			exit: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, g := layerGuard(t, layerFixture(t, tc.lists, tc.files))
			wantStatus := "pass"
			if tc.exit == 1 {
				wantStatus = "fail"
			}
			if env.Stages[0].ExitCode != tc.exit || g.Status != wantStatus {
				t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
			}
			if tc.excerpt != "" && !strings.Contains(g.Excerpt, tc.excerpt) {
				t.Fatalf("excerpt missing %q: %q", tc.excerpt, g.Excerpt)
			}
			if tc.exit == 0 && (!strings.Contains(g.Summary, layerChainText) || g.Violations != 0 || g.ExitCode != 0) {
				t.Fatalf("passing summary wrong: %+v", g)
			}
			if tc.exit == 1 && g.ExitCode != 1 {
				t.Fatalf("violation exit code wrong: %+v", g)
			}
		})
	}
}

var layerUpwardTest = map[string]string{
	"internal/agent/b/pkg.go": pkgFile("b"),
	"internal/serve/c/pkg.go": pkgFile("c"),
	"internal/kit/pkg.go":     pkgFile("a"),
	"internal/kit/a_test.go":  "package a\n\nimport \"fixture/internal/agent/b\"\n",
}

func TestLayersTestFileRatchet(t *testing.T) {
	t.Run("unrecorded test violation fails", func(t *testing.T) {
		env, g := layerGuard(t, layerFixture(t, nil, layerUpwardTest))
		if env.Stages[0].ExitCode != 1 || g.Status != "fail" || g.Violations != 2 || !strings.Contains(g.Excerpt, "fix the test") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
	t.Run("recorded at its count passes", func(t *testing.T) {
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_test_exceptions.txt": "internal/kit/a_test.go 1\n"}, layerUpwardTest))
		if env.Stages[0].ExitCode != 0 || g.Status != "pass" {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
	t.Run("grown past recorded count fails", func(t *testing.T) {
		files := map[string]string{}
		for p, c := range layerUpwardTest {
			files[p] = c
		}
		files["internal/kit/a_test.go"] = "package a\n\nimport (\n\t\"fixture/internal/agent/b\"\n\t\"fixture/internal/serve/c\"\n)\n"
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_test_exceptions.txt": "internal/kit/a_test.go 1\n"}, files))
		if env.Stages[0].ExitCode != 1 || !strings.Contains(g.Excerpt, "RATCHET VIOLATION: internal/kit/a_test.go now violates with 2 import(s) (recorded 1)") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
}

func TestLayersStaleAndLiveExceptions(t *testing.T) {
	t.Run("stale production exception fails", func(t *testing.T) {
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_exceptions.txt": "fixture/internal/kit/a 1\n"}, map[string]string{
			"internal/agent/b/pkg.go": pkgFile("b"),
			"internal/kit/a/pkg.go":   pkgFile("a"),
		}))
		if env.Stages[0].ExitCode != 1 || !strings.Contains(g.Excerpt, "remove it from") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
	t.Run("stale test exception in a live package dir fails with cleanup", func(t *testing.T) {
		// The Go port normalizes paths (no Windows backslash artifact), so
		// a test file that no longer exists in a live package dir is
		// RATCHET CLEANUP, exactly as the shell's logic reads on
		// forward-slash paths; see the report notes.
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_test_exceptions.txt": "internal/kit/a/gone_test.go 1\n"}, map[string]string{
			"internal/kit/a/pkg.go": pkgFile("a"),
		}))
		if env.Stages[0].ExitCode != 1 || !strings.Contains(g.Excerpt, "RATCHET CLEANUP: internal/kit/a/gone_test.go") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
	t.Run("stale test exception with a vanished package dir fails with STALE EXCEPTION", func(t *testing.T) {
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_test_exceptions.txt": "internal/kit/gonepkg/gone_test.go 1\n"}, map[string]string{
			"internal/kit/a/pkg.go": pkgFile("a"),
		}))
		if env.Stages[0].ExitCode != 1 || !strings.Contains(g.Excerpt, "STALE EXCEPTION: internal/kit/gonepkg/gone_test.go") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
	t.Run("live production exception at its count passes", func(t *testing.T) {
		env, g := layerGuard(t, layerFixture(t, map[string]string{"layer_exceptions.txt": "fixture/internal/kit/a 1\n"}, map[string]string{
			"internal/agent/b/pkg.go": pkgFile("b"),
			"internal/kit/a/pkg.go":   pkgFile("a", "fixture/internal/agent/b"),
		}))
		if env.Stages[0].ExitCode != 0 || !strings.Contains(g.Summary, "1 production edge(s) allowlisted (kept)") {
			t.Fatalf("exit=%d guard=%+v", env.Stages[0].ExitCode, g)
		}
	})
}

// TestLayersViolationMessages asserts the exact verdict wording the
// shell prints, via a canned `go list` stream (the messages themselves
// never reach the excerpt in full for multi-verdict runs, so they are
// checked at the guard boundary).
func TestLayersViolationMessages(t *testing.T) {
	dir := layerFixture(t, nil, map[string]string{
		"internal/agent/b/pkg.go": pkgFile("b"),
		"internal/serve/c/pkg.go": pkgFile("c"),
		"internal/kit/pkg.go":     pkgFile("a"),
		"internal/kit/a_test.go":  "package a\n\nimport \"fixture/internal/agent/b\"\n",
	})
	list := `{"ImportPath":"fixture/internal/agent/b","Dir":"` + filepath.ToSlash(dir) + `/internal/agent/b"}
{"ImportPath":"fixture/internal/serve/c","Dir":"` + filepath.ToSlash(dir) + `/internal/serve/c"}
{"ImportPath":"fixture/internal/kit","Dir":"` + filepath.ToSlash(dir) + `/internal/kit","TestImports":["fixture/internal/agent/b"]}
`
	invoked := 0
	invoke := func(cwd string, env []string, args ...string) ([]byte, error) {
		invoked++
		if invoked == 1 {
			return []byte("fixture\n"), nil
		}
		return []byte(list), nil
	}
	r, err := static.RunLayers(dir, invoke)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "fail" || r.Violations != 2 {
		t.Fatalf("guard=%+v", r)
	}
	if r.Directories != 3 || r.Files != 3 {
		t.Fatalf("examined counts wrong: %+v", r)
	}
	if !strings.Contains(r.Excerpt, "fix the test, or record it in layer_test_exceptions.txt as \"internal/kit/a_test.go 1\"") {
		t.Fatalf("test-file hint missing: %q", r.Excerpt)
	}
	// The full verdict list carries the class messages verbatim.
	want := []string{
		"fix the test, or record it in layer_test_exceptions.txt as \"internal/kit/a_test.go 1\"",
		"TEST LAYER VIOLATION: internal/kit/a_test.go imports fixture/internal/agent/b (group kit test may not import group agent)",
	}
	if len(r.Messages) != len(want) {
		t.Fatalf("messages=%q", r.Messages)
	}
	for i, m := range want {
		if r.Messages[i] != m {
			t.Fatalf("message %d = %q want %q", i, r.Messages[i], m)
		}
	}
	// The production-violation run names importer, target and both groups.
	prod := `{"ImportPath":"fixture/internal/agent/b","Dir":"` + filepath.ToSlash(dir) + `/internal/agent/b"}
{"ImportPath":"fixture/internal/kit/a","Dir":"` + filepath.ToSlash(dir) + `/internal/kit/a","Imports":["fixture/internal/agent/b"]}
`
	n := 0
	prodRes, err := static.RunLayers(dir, func(cwd string, env []string, args ...string) ([]byte, error) {
		n++
		if n%2 == 1 {
			return []byte("fixture\n"), nil
		}
		return []byte(prod), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prodRes.Messages) != 2 || prodRes.Messages[1] != "LAYER VIOLATION: fixture/internal/kit/a (group kit) imports fixture/internal/agent/b (group agent)" || !strings.Contains(prodRes.Messages[0], "\"fixture/internal/kit/a 1\"") {
		t.Fatalf("prod messages=%q", prodRes.Messages)
	}
}

func TestLayersUnknownGroupIsBrokenInput(t *testing.T) {
	env, g := layerGuard(t, layerFixture(t, nil, map[string]string{"internal/foo/x/pkg.go": pkgFile("x")}))
	if env.Stages[0].ExitCode != 3 || g.Status != "error" || g.ExitCode != 3 || !strings.Contains(env.Stages[0].Excerpt, "fixture/internal/foo/x") {
		t.Fatalf("exit=%d stage=%+v guard=%+v", env.Stages[0].ExitCode, env.Stages[0], g)
	}
	if env.OK || env.Stages[0].Status != "error" {
		t.Fatalf("broken input must not pass: %+v", env.Stages[0])
	}
}

func TestLayersGoListFailureIsBrokenInput(t *testing.T) {
	root := layerFixture(t, nil, map[string]string{"src/pkg.go": pkgFile("fixture")})
	// Corrupt go.mod so `go list` itself fails: broken input -> exit 3.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("this is not a go.mod file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _ := layerGuard(t, root)
	if env.Stages[0].ExitCode != 3 || env.OK || env.Stages[0].Status != "error" {
		t.Fatalf("exit=%d stage=%+v", env.Stages[0].ExitCode, env.Stages[0])
	}
	if !strings.Contains(env.Stages[0].Excerpt, "go list") || len(env.Stages[0].Guards) != 7 {
		t.Fatalf("broken go list not reported: %+v", env.Stages[0])
	}
	// P1-b: the excerpt must carry Go's OWN diagnostic (captured from
	// *exec.ExitError.Stderr -- "go: errors parsing go.mod: go.mod:1:
	// unknown directive: this"), not just the runner's "go list failed".
	if !strings.Contains(env.Stages[0].Excerpt, "unknown directive") || !strings.Contains(env.Stages[0].Excerpt, "errors parsing go.mod") {
		t.Fatalf("excerpt must contain go's own diagnostic: %q", env.Stages[0].Excerpt)
	}
}

// TestGoEnvHardensChildEnvironment pins the hardened child environment
// through the recording seam: GOFLAGS= is set EMPTY (overriding `go env
// -w` saved flags), GOMEMLIMIT is capped, and no non-empty GOFLAGS
// leaks in.
func TestGoEnvHardensChildEnvironment(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=leaky -p 4")
	var recorded []string
	root := layerFixture(t, nil, map[string]string{"internal/kit/a/pkg.go": pkgFile("a")})
	_, err := static.RunLayers(root, func(cwd string, env []string, args ...string) ([]byte, error) {
		_ = cwd
		_ = args
		recorded = env
		if len(args) > 1 && args[len(args)-1] == "-m" {
			return []byte("fixture\n"), nil
		}
		list := `{"ImportPath":"fixture/internal/kit/a","Dir":"` + filepath.ToSlash(root) + `/internal/kit/a"}` + "\n"
		return []byte(list), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawEmptyFlags, sawMemLimit bool
	for _, kv := range recorded {
		switch {
		case kv == "GOFLAGS=":
			sawEmptyFlags = true
		case strings.HasPrefix(kv, "GOFLAGS=") && kv != "GOFLAGS=":
			t.Fatalf("non-empty GOFLAGS leaked into child env: %q", kv)
		case kv == "GOMEMLIMIT=700MiB":
			sawMemLimit = true
		}
	}
	if !sawEmptyFlags || !sawMemLimit {
		t.Fatalf("env must contain GOFLAGS= and GOMEMLIMIT=700MiB; sawEmptyFlags=%v sawMemLimit=%v", sawEmptyFlags, sawMemLimit)
	}
}
