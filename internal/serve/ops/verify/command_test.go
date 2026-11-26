package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	static "github.com/PHPCraftdream/rush/internal/serve/ops/verify/static"
)

func initFixture(t *testing.T, files map[string][]byte, lists map[string]string) string {
	return initFixtureOpt(t, files, lists, true)
}

// initFixtureOpt builds the fixture; companion=false omits the probe
// test file, for tests that specifically need ZERO tracked *_test.go.
func initFixtureOpt(t *testing.T, files map[string][]byte, lists map[string]string, companion bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt", "layer_exceptions.txt", "layer_test_exceptions.txt", "pkg_names_allowlist.txt"} {
		if err := os.WriteFile(filepath.Join(root, ".githooks", n), []byte(lists[n]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for p, b := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Every fixture carries one REAL tracked *_test.go file at the repo
	// root (where the directory guards never count it) so the
	// db-release-pairing guard examines a non-zero universe and the
	// run is not vacuously converted to "examined no applicable
	// inputs". Fixtures that seed their own *_test.go are left alone.
	if companion && !hasTestFile(files) {
		// A dedicated subdirectory keeps the probe out of the root
		// package (fixtures with a root main.go or root package would
		// otherwise collide in go list) and outside the directory
		// guards' counted parents.
		if err := os.MkdirAll(filepath.Join(root, "guardprobe"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "guardprobe", "p_test.go"), []byte("package guardprobe\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := platform.Command(context.Background(), "git", "init", "-q")
	c.Dir = root
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git init: %s %v", b, e)
	}
	c = platform.Command(context.Background(), "git", "add", "-A")
	c.Dir = root
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git add: %s %v", b, e)
	}
	c = platform.Command(context.Background(), "git", "-c", "user.name=Verify Test", "-c", "user.email=verify-test@example.invalid", "commit", "-q", "--allow-empty", "-m", "fixture")
	c.Dir = root
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git commit: %s %v", b, e)
	}
	return root
}

func goFiles(n int) map[string][]byte {
	m := map[string][]byte{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("src/f%02d.go", i)] = []byte("package fixture\n")
	}
	return m
}

func hasTestFile(files map[string][]byte) bool {
	for p := range files {
		if strings.HasSuffix(p, "_test.go") {
			return true
		}
	}
	return false
}

func runCobra(t *testing.T, root string, args ...string) (int, []byte, error) {
	t.Helper()
	cmd := NewCommand()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"guards", root}, args...))
	err := cmd.Execute()
	if err == nil {
		return 0, out.Bytes(), nil
	}
	if e, ok := err.(interface{ ExitCode() int }); ok {
		return e.ExitCode(), out.Bytes(), err
	}
	t.Fatalf("untyped error %v stderr=%s", err, stderr.String())
	return -1, nil, err
}

func filesWithCount(count int, includeTests bool) map[string][]byte {
	m := map[string][]byte{}
	for i := 0; i < count; i++ {
		suffix := ".go"
		if includeTests && i >= 3 {
			suffix = "_test.go"
		}
		m[fmt.Sprintf("src/f%02d%s", i, suffix)] = []byte("package fixture\n")
	}
	return m
}

func guardIndex(guards []Guard, name string) int {
	for i, g := range guards {
		if g.Name == name {
			return i
		}
	}
	return -1
}

func TestCobraUnbornStagedRepositoryRunsAllGuards(t *testing.T) {
	root := t.TempDir()
	for p, b := range goFiles(3) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "guardprobe"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "guardprobe", "p_test.go"), []byte("package guardprobe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt", "layer_exceptions.txt", "layer_test_exceptions.txt", "pkg_names_allowlist.txt"} {
		if err := os.WriteFile(filepath.Join(root, ".githooks", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"git", "init", "-q"}, {"git", "add", "-A"}} {
		cmd := platform.Command(context.Background(), argv[0], argv[1:]...)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", argv, b, err)
		}
	}
	code, out, err := runCobra(t, root, "--json")
	if code != 0 {
		t.Fatalf("unborn staged repo exit=%d err=%v", code, err)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	// This comparison covers the four pure guards; the toolchain-backed
	// guards get a canned go list so Run completes and returns seven
	// results aligned with the envelope's.
	spawns := 0
	want, err := static.Run(root, []string{"src/f00.go", "src/f01.go", "src/f02.go"}, false, func(cwd string, env []string, args ...string) ([]byte, error) {
		spawns++
		if spawns%2 == 1 {
			return []byte("fixture\n"), nil
		}
		list := `{"ImportPath":"fixture/src","Dir":"` + filepath.ToSlash(cwd) + `/src"}` + "\n"
		return []byte(list), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.Commit != "none" || len(env.Stages[0].Guards) != 7 || len(want) != 7 {
		t.Fatalf("unexpected result %+v", env)
	}
	for i, got := range env.Stages[0].Guards[:4] {
		if got.Status != want[i].Status || got.Violations != want[i].Violations {
			t.Fatalf("guard %s got %+v want %+v", got.Name, got, want[i])
		}
	}
}

func TestCobraGuardViolationCasesAndGoldens(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		lists   map[string]string
		guard   string
		exit    int
		excerpt string
	}{{"pass", goFiles(3), nil, "", 0, ""}, {"entries", goFiles(8), nil, "dir-entries", 1, "TOO MANY ENTRIES: src has 8 elements"}, {"files", goFiles(22), nil, "dir-files", 1, "TOO MANY FILES: src has 22 code files"}, {"size1000", map[string][]byte{"src/big.go": []byte("package fixture\n" + strings.Repeat("x\n", 1000))}, nil, "file-size", 1, "TOO LONG: src/big.go is 1001 lines"}, {"size900", map[string][]byte{"src/big.ts": []byte(strings.Repeat("x\n", 901)), "src/big.go": []byte("package fixture\n")}, nil, "file-size-900", 1, "OVER 900: src/big.ts is 901 lines"}, {"entriesNested", func() map[string][]byte {
		m := goFiles(7)
		m["src/nested/a.go"] = []byte("package fixture\n")
		return m
	}(), nil, "dir-entries", 1, "TOO MANY ENTRIES: src has 8 elements"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initFixture(t, tc.files, tc.lists)
			exit, out, err := runCobra(t, root, "--json")
			if exit != tc.exit {
				t.Fatalf("exit=%d expected=%d err=%v", exit, tc.exit, err)
			}
			var env Envelope
			if e := json.Unmarshal(out, &env); e != nil {
				t.Fatalf("invalid JSON: %v", e)
			}
			if len(env.Stages) != 1 || env.Stages[0].Name != "guards" || len(env.Stages[0].Guards) != 7 {
				t.Fatalf("bad stage structure: %+v", env.Stages)
			}
			if tc.guard != "" {
				i := guardIndex(env.Stages[0].Guards, tc.guard)
				if i < 0 || env.Stages[0].Guards[i].Status != "fail" || !strings.Contains(env.Stages[0].Guards[i].Excerpt, tc.excerpt) {
					t.Fatalf("failure lost: %+v", env.Stages[0].Guards)
				}
			} else if !env.OK {
				t.Fatalf("clean fixture failed: %+v", env)
			}
			if tc.name == "pass" || tc.name == "entriesNested" {
				env.Target = "<TARGET>"
				env.Commit = "<COMMIT>"
				env.Stages[0].DurationMS = 0
				for i := range env.Stages[0].Guards {
					env.Stages[0].Guards[i].DurationMS = 0
				}
				for i := range env.Stages {
					for j := range env.Stages[i].Provenance {
						env.Stages[i].Provenance[j].CWD = "<TARGET>"
					}
				}
				b, e := Encode(env)
				if e != nil {
					t.Fatal(e)
				}
				text := strings.ReplaceAll(string(b)+"\n", root, "<TARGET>")
				text = strings.ReplaceAll(text, env.Commit, "<COMMIT>")
				text = strings.ReplaceAll(text, env.StartedAt, "<TIME>")
				text = strings.ReplaceAll(text, env.FinishedAt, "<TIME>")
				path := filepath.Join("testdata", "guards_"+tc.name+".golden")
				if os.Getenv("UPDATE_GOLDEN") == "1" {
					if e := os.WriteFile(path, []byte(text), 0o600); e != nil {
						t.Fatal(e)
					}
				}
				want, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				if string(want) != text {
					t.Fatalf("golden mismatch: %s", path)
				}
			}
		})
	}
}

func TestCobraRatchetGrowthBothEqualAndGrowth(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string][]byte
		allow       map[string]string
		key         string
		record      int
		guard       string
		growthValue int
	}{
		{"dir-entries", goFiles(10), map[string]string{"dir_entries_allowlist.txt": "src %d\n"}, "src", 10, "dir-entries", 9},
		{"dir-files", filesWithCount(22, true), map[string]string{"dir_files_allowlist.txt": "src %d\n"}, "src", 22, "dir-files", 21},
		{"size1000", map[string][]byte{"src/big.go": []byte("package fixture\n" + strings.Repeat("x\n", 1099))}, map[string]string{"file_size_allowlist.txt": "src/big.go %d\n", "file_size_900_allowlist.txt": "src/big.go 1300\n"}, "src/big.go", 1100, "file-size", 100},
		{"size900", map[string][]byte{"src/big.ts": []byte(strings.Repeat("x\n", 950)), "src/big.go": []byte("package fixture\n")}, map[string]string{"file_size_900_allowlist.txt": "src/big.ts %d\n"}, "src/big.ts", 950, "file-size-900", 25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for delta := 1; delta >= -1; delta -= 2 {
				lists := map[string]string{}
				for name, content := range tc.allow {
					if strings.Contains(content, "%d") {
						content = fmt.Sprintf(content, tc.record+(delta-1))
					}
					lists[name] = content
				}
				root := initFixture(t, tc.files, lists)
				code, out, err := runCobra(t, root, "--json")
				if delta == 1 {
					if code != 0 {
						t.Fatalf("at allowance count should pass: exit=%d err=%v", code, err)
					}
					continue
				}
				if code != 1 {
					t.Fatalf("ratchet growth should fail: exit=%d err=%v", code, err)
				}
				var env Envelope
				if e := json.Unmarshal(out, &env); e != nil {
					t.Fatal(e)
				}
				i := guardIndex(env.Stages[0].Guards, tc.guard)
				if i < 0 || env.Stages[0].Guards[i].Status != "fail" {
					t.Fatalf("growth not reported by %s", tc.guard)
				}
			}
		})
	}
}

func TestCobraRatchetDropToLimitCleanup(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		lists   map[string]string
		guard   string
		excerpt string
	}{
		{"dir-entries", goFiles(7), map[string]string{"dir_entries_allowlist.txt": "src 8\n"}, "dir-entries", "RATCHET CLEANUP: src is now 7 elements"},
		{"dir-files", filesWithCount(21, true), map[string]string{"dir_files_allowlist.txt": "src 22\n"}, "dir-files", "RATCHET CLEANUP: src is now 21 code files"},
		{"file-size-1000", map[string][]byte{"src/big.go": []byte("package fixture\n" + strings.Repeat("x\n", 999))}, map[string]string{"file_size_allowlist.txt": "src/big.go 1100\n", "file_size_900_allowlist.txt": "src/big.go 1200\n"}, "file-size", "RATCHET CLEANUP: src/big.go is now 1000 lines"},
		{"file-size-900", map[string][]byte{"src/big.ts": []byte(strings.Repeat("x\n", 900)), "src/big.go": []byte("package fixture\n")}, map[string]string{"file_size_900_allowlist.txt": "src/big.ts 950\n"}, "file-size-900", "RATCHET CLEANUP: src/big.ts is now 900 lines"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initFixture(t, tc.files, tc.lists)
			code, output, err := runCobra(t, root, "--json")
			if code != 1 {
				t.Fatalf("cleanup must fail via command: exit=%d err=%v", code, err)
			}
			var envelope Envelope
			if e := json.Unmarshal(output, &envelope); e != nil {
				t.Fatal(e)
			}
			if len(envelope.Stages) != 1 || len(envelope.Stages[0].Guards) != 7 {
				t.Fatalf("unexpected result shape: %+v", envelope)
			}
			index := guardIndex(envelope.Stages[0].Guards, tc.guard)
			if index < 0 {
				t.Fatalf("missing cleanup guard %s", tc.guard)
			}
			result := envelope.Stages[0].Guards[index]
			if result.Status != "fail" || result.ExitCode != 1 || !strings.Contains(result.Excerpt, tc.excerpt) {
				t.Fatalf("expected cleanup failure for %s, got %+v", tc.guard, result)
			}
		})
	}
}

func TestCobraGuardVerdictBoundariesAndIgnoredPaths(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string][]byte
		wantExit int
		guard    string
		lists    map[string]string
	}{
		{"dir-files-21", filesWithCount(21, true), 0, "dir-files", nil},
		{"size-1000-exact", map[string][]byte{"src/exact.go": []byte("package fixture\n" + strings.Repeat("x\n", 999))}, 0, "file-size", map[string]string{"file_size_900_allowlist.txt": "src/exact.go 1000\n"}},
		{"size-900-exact", map[string][]byte{"src/exact.ts": []byte(strings.Repeat("x\n", 900)), "src/exact.go": []byte("package fixture\n")}, 0, "file-size-900", nil},
		{"generated-and-ignored", map[string][]byte{"src/a.go": []byte("package fixture\n"), "src/gen.go": []byte("// Code generated; DO NOT EDIT.\npackage fixture\n" + strings.Repeat("x\n", 1000)), "src/testdata/x.go": []byte(strings.Repeat("x\n", 1100)), "src/node_modules/y.ts": []byte(strings.Repeat("x\n", 1100)), "src/dist/z.tsx": []byte(strings.Repeat("x\n", 1100)), "src/0real.go": []byte("package fixture\n" + strings.Repeat("x\n", 1000))}, 1, "file-size", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initFixture(t, tc.files, tc.lists)
			code, out, err := runCobra(t, root, "--json")
			if code != tc.wantExit {
				t.Fatalf("exit=%d want=%d err=%v", code, tc.wantExit, err)
			}
			var env Envelope
			if e := json.Unmarshal(out, &env); e != nil {
				t.Fatalf("invalid JSON: %v", e)
			}
			i := guardIndex(env.Stages[0].Guards, tc.guard)
			if i < 0 {
				t.Fatalf("missing %s", tc.guard)
			}
			if env.Stages[0].Guards[i].Status != "pass" && tc.name != "generated-and-ignored" {
				t.Fatalf("boundary/ignored fixture failed: %+v", env.Stages[0].Guards[i])
			}
			if tc.name == "generated-and-ignored" {
				if env.Stages[0].Guards[i].Status != "fail" || !strings.Contains(env.Stages[0].Guards[i].Excerpt, "src/0real.go") || strings.Contains(env.Stages[0].Guards[i].Excerpt, "gen.go") || strings.Contains(env.Stages[0].Guards[i].Excerpt, "testdata") || strings.Contains(env.Stages[0].Guards[i].Excerpt, "node_modules") || strings.Contains(env.Stages[0].Guards[i].Excerpt, "dist") {
					t.Fatalf("skips/real violation not proven: %+v", env.Stages[0].Guards[i])
				}
			}
		})
	}
}

func TestCobraZeroDirectoriesFailsClosed(t *testing.T) {
	// companion=false: this test is specifically about vacuous inputs,
	// so the probe *_test.go must not create a directory.
	root := initFixtureOpt(t, map[string][]byte{"root.go": []byte("package fixture\\n")}, nil, false)
	code, out, err := runCobra(t, root, "--json")
	if code != 3 {
		t.Fatalf("want exit 3; got %d err=%v", code, err)
	}
	var env Envelope
	if e := json.Unmarshal(out, &env); e != nil {
		t.Fatalf("invalid result JSON: %v", e)
	}
	if len(env.Stages) != 1 || len(env.Stages[0].Guards) != 7 {
		t.Fatalf("missing per-guard results: %+v", env)
	}
	entries := env.Stages[0].Guards[0]
	if entries.Status != "error" || entries.ExitCode != 3 || entries.Directories != 0 || entries.Files != 0 {
		t.Fatalf("entry guard should fail closed with zero dirs/files: %+v", entries)
	}
	if env.Stages[0].Status != "error" || env.Stages[0].ExitCode != 3 || env.OK {
		t.Fatalf("stage contract violated: %+v", env.Stages[0])
	}
}

func TestCobraNonGitEnvelopeRecordsOnlyStartedCommand(t *testing.T) {
	root := t.TempDir()
	code, out, _ := runCobra(t, root, "--json")
	if code != 3 {
		t.Fatalf("exit=%d", code)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	p := env.Stages[0].Provenance
	if len(p) != 1 || p[0].CWD != root || strings.Join(p[0].Args, " ") != "git rev-parse --show-toplevel" || p[0].ExitCode == 0 {
		t.Fatalf("fabricated/missing provenance: %+v", p)
	}
}

func TestCobraOversizedExcerptBounded(t *testing.T) {
	root := initFixture(t, goFiles(3), map[string]string{"dir_entries_allowlist.txt": strings.Repeat("x", 6000)})
	code, out, _ := runCobra(t, root, "--json")
	if code != 3 {
		t.Fatalf("exit=%d", code)
	}
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	excerpt := env.Stages[0].Excerpt
	if len(excerpt) > excerptLimit || !strings.Contains(excerpt, excerptMarker) {
		t.Fatalf("excerpt not byte-bounded with marker: len=%d %q", len(excerpt), excerpt[len(excerpt)-30:])
	}
}

func TestCobraBrokenInputsEachMissingAllowlist(t *testing.T) {
	for _, missing := range []string{"dir_entries_allowlist.txt", "dir_files_allowlist.txt", "file_size_allowlist.txt", "file_size_900_allowlist.txt", "layer_exceptions.txt", "layer_test_exceptions.txt", "pkg_names_allowlist.txt"} {
		t.Run(missing, func(t *testing.T) {
			root := initFixture(t, goFiles(3), nil)
			if err := os.Remove(filepath.Join(root, ".githooks", missing)); err != nil {
				t.Fatal(err)
			}
			code, out, _ := runCobra(t, root, "--json")
			if code != 3 {
				t.Fatalf("missing %s exit=%d want 3", missing, code)
			}
			var env Envelope
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatal(err)
			}
			if len(env.Stages) != 1 || env.Stages[0].ExitCode != 3 || len(env.Stages[0].Guards) != 7 {
				t.Fatalf("broken missing input not reported: %+v", env)
			}
		})
	}
}

func TestCobraExitThreeBrokenTargets(t *testing.T) {
	cases := []struct {
		name   string
		root   string
		files  map[string][]byte
		lists  map[string]string
		nonGit bool
	}{{"malformed", "", goFiles(3), map[string]string{"dir_entries_allowlist.txt": "broken\n"}, false}, {"empty", "", map[string][]byte{}, nil, false}, {"vacuous", "", map[string][]byte{"README.md": []byte("empty\n")}, nil, false}, {"not-git", "", nil, nil, true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var root string
			if !tc.nonGit {
				root = initFixture(t, tc.files, tc.lists)
			} else {
				root = t.TempDir()
			}
			code, out, _ := runCobra(t, root, "--json")
			if code != 3 {
				t.Fatalf("want exit 3, got %d", code)
			}
			var env Envelope
			if e := json.Unmarshal(out, &env); e != nil {
				t.Fatalf("error JSON invalid: %v", e)
			}
			if len(env.Stages) != 1 || len(env.Stages[0].Guards) != 7 {
				t.Fatalf("broken result omitted per-check records: %+v", env)
			}
		})
	}
}

func TestCobraIncludesTestsAndDefaultSubdirectory(t *testing.T) {
	t.Run("count-tests", func(t *testing.T) {
		m := goFiles(7)
		for i := 0; i < 12; i++ {
			m[fmt.Sprintf("src/t%d_test.go", i)] = []byte("package fixture\n")
		}
		root := initFixture(t, m, nil)
		t.Setenv("DIR_ENTRIES_COUNT_TESTS", "1")
		code, out, _ := runCobra(t, root, "--json")
		if code != 1 {
			t.Fatalf("count tests exit %d", code)
		}
		var env Envelope
		if e := json.Unmarshal(out, &env); e != nil {
			t.Fatal(e)
		}
		if env.Stages[0].Guards[0].Status != "fail" {
			t.Fatal("test files not counted")
		}
	})
	t.Run("subdirectory", func(t *testing.T) {
		root := initFixture(t, goFiles(3), nil)
		sub := filepath.Join(root, "src")
		old, e := os.Getwd()
		if e != nil {
			t.Fatal(e)
		}
		if e = os.Chdir(sub); e != nil {
			t.Fatal(e)
		}
		defer os.Chdir(old)
		cmd := NewCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"guards", "--json"})
		if e = cmd.Execute(); e != nil {
			t.Fatal(e)
		}
		var env Envelope
		if e = json.Unmarshal(out.Bytes(), &env); e != nil {
			t.Fatal(e)
		}
		if !sameDirectory(env.Target, root) {
			t.Fatalf("target=%s want=%s", env.Target, root)
		}
		if len(env.Stages[0].Provenance) == 0 || !sameDirectory(env.Stages[0].Provenance[0].CWD, sub) {
			t.Fatalf("subdirectory command cwd lost: %+v", env.Stages[0].Provenance)
		}
	})
}

// sameDirectory compares directories by identity: Windows runners hand out
// 8.3 short names (RUNNER~1) for the same temporary directory.
func sameDirectory(a, b string) bool {
	left, errA := os.Stat(a)
	right, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(left, right)
}
