package verify

// Command-level port of .githooks/check_db_release_pairing_selftest.sh
// plus seeded violation/pairing fixtures. The selftest's mock-git cases
// (forced git grep exit statuses and a canned candidate list) are
// reproduced at the static guard boundary with a fake InvokeFunc, the
// same substitution the shell selftest performs with its BASH_ENV git
// function; all other cases run the real command code path end to end.

import (
	"errors"
	"strings"
	"testing"

	static "github.com/PHPCraftdream/rush/internal/serve/ops/verify/static"
)

// The repository's db-release-pairing guard counts these call spellings in
// every tracked *_test.go, this one included, so the fixtures build them at
// run time instead of containing them literally.
const (
	dbConnect     = "db." + "Connect("
	dbConnectRead = "db." + "ConnectRead("
	dbRelease     = "db." + "Release("
	dbReleaseAll  = "db." + "ReleaseAll("
	dbReleaseConn = "db." + "ReleaseConn("
)

func TestDbPairingSeededViolationsThroughCommand(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		exit    int
		excerpt string
	}{
		{
			name: "ReleaseConn pairs one Connect",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\nfunc f() { " + dbConnect + "ctx, d); " + dbReleaseConn + "conn) }\n"),
			},
			exit: 0,
		},
		{
			name: "one ReleaseConn cannot pair two connections",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\nfunc f() { " + dbConnect + "ctx, d); " + dbConnectRead + "ctx, d); " + dbReleaseConn + "conn) }\n"),
			},
			exit:    1,
			excerpt: "src/p_test.go: 2 db.Connect/ConnectRead call(s), 1 db.Release/ReleaseAll/ReleaseConn call(s)",
		},
		{
			name: "an unpaired connection fails",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\nfunc f() { " + dbConnect + "ctx, d) }\n"),
			},
			exit:    1,
			excerpt: "src/p_test.go: 1 db.Connect/ConnectRead call(s), 0 db.Release/ReleaseAll/ReleaseConn call(s)",
		},
		{
			name: "a paired connect passes",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\nfunc f() { " + dbConnectRead + "ctx, d); " + dbRelease + "d) }\n"),
			},
			exit: 0,
		},
		{
			name: "full-line comments do not count",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\n// " + dbConnect + "ctx, d) is explained in prose\nfunc f() { " + dbConnect + "ctx, d); " + dbReleaseAll + "d) }\n"),
			},
			exit: 0,
		},
		{
			name: "zero matching candidates stays a pass",
			files: map[string][]byte{
				"src/p_test.go": []byte("package p\n\nfunc f() { t.Cleanup(func() {}) }\n"),
			},
			exit: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A companion tracked non-test .go file keeps the pkg-names
			// guard's examined-nothing fail-closed rule quiet; this test
			// is about db pairing.
			files := map[string][]byte{"src/p.go": []byte("package p\n")}
			for p, c := range tc.files {
				files[p] = c
			}
			root := initFixture(t, files, nil)
			code, out, err := runCobra(t, root, "--json")
			if code != tc.exit {
				t.Fatalf("exit=%d want=%d err=%v", code, tc.exit, err)
			}
			env := decodeEnvelope(t, out)
			g := guardByName(t, env, "db-release-pairing")
			wantStatus := "pass"
			if tc.exit == 1 {
				wantStatus = "fail"
			}
			if g.Status != wantStatus || g.ExitCode != tc.exit {
				t.Fatalf("status=%s exit=%d want %s/%d", g.Status, g.ExitCode, wantStatus, tc.exit)
			}
			if tc.excerpt != "" && !strings.Contains(g.Excerpt, tc.excerpt) {
				t.Fatalf("excerpt missing %q: %q", tc.excerpt, g.Excerpt)
			}
			if tc.exit == 1 && !strings.Contains(g.Summary, "1 unpaired") {
				t.Fatalf("summary must count the violation: %+v", g)
			}
			if tc.exit == 0 && g.Violations != 0 {
				t.Fatalf("clean fixture reported violations: %+v", g)
			}
		})
	}
}

type fakeExitErr struct{ code int }

func (e *fakeExitErr) Error() string { return "exit status" }
func (e *fakeExitErr) ExitCode() int { return e.code }

func TestDbPairingGrepFailureClasses(t *testing.T) {
	t.Run("git grep hard failure (exit 128) is broken input: error verdict, failed-closed excerpt", func(t *testing.T) {
		root := initFixture(t, map[string][]byte{"src/p_test.go": []byte("package p\n")}, nil)
		r := runDbPairFake(root, &fakeExitErr{128})
		// Error verdicts are what the verify runner maps to exit 3 (see
		// Run's status switch); the shell exits 1 here, the port fails
		// closed with 3 by design.
		if r.Status != "error" || !strings.Contains(r.Excerpt, "git grep failed") || !strings.Contains(r.Excerpt, "failed closed") {
			t.Fatalf("guard=%+v", r)
		}
		if !strings.Contains(r.Summary, "failed closed") {
			t.Fatalf("summary must say the guard failed closed: %+v", r)
		}
	})
	t.Run("git grep spawn failure (non-ExitError) is broken input too", func(t *testing.T) {
		root := initFixture(t, map[string][]byte{"src/p_test.go": []byte("package p\n")}, nil)
		r := runDbPairFake(root, errors.New("exec: git: executable file not found"))
		if r.Status != "error" || !strings.Contains(r.Excerpt, "git grep failed") {
			t.Fatalf("guard=%+v", r)
		}
	})
	t.Run("git grep exit 1 (zero matches) stays a pass", func(t *testing.T) {
		root := initFixture(t, map[string][]byte{"src/p_test.go": []byte("package p\n")}, nil)
		r := runDbPairFake(root, &fakeExitErr{1})
		if r.Status != "pass" || r.Violations != 0 {
			t.Fatalf("guard=%+v", r)
		}
	})
	t.Run("exempted candidate file is skipped: a real tracked fixture with an unpaired connect stays a pass", func(t *testing.T) {
		// The exemption fixture is REAL tracked content: without the
		// exemption, this file's unpaired db.Connect would fail the
		// guard (proven by the byte-identical non-exempt sibling below,
		// which the same run flags).
		unpaired := []byte("package p\n\nfunc f() { " + dbConnect + "ctx, d) }\n")
		root := initFixture(t, map[string][]byte{
			"internal/app/shutdown_result_test.go": unpaired,
			"other/sibling_test.go":                unpaired,
		}, nil)
		listing := "internal/app/shutdown_result_test.go\nother/sibling_test.go\n"
		r := runDbPairFakeListing(root, listing, []string{"internal/app/shutdown_result_test.go", "other/sibling_test.go"})
		if r.Status != "fail" || len(r.Messages) != 1 || !strings.Contains(r.Messages[0], "other/sibling_test.go") {
			t.Fatalf("identical non-exempt content must fail while the exempt file is skipped: %+v", r)
		}
		if strings.Contains(strings.Join(r.Messages, "\n"), "shutdown_result_test.go") {
			t.Fatalf("exempted file must not be flagged: %+v", r)
		}
	})
	// Command-level: tracked tests without calls are a legitimate pass
	// (exit 0). The seam-level git-grep-exit-1 zero-match case stays a
	// pass two subtests down.
	t.Run("tracked tests without db calls are a legitimate pass", func(t *testing.T) {
		// The companion non-test file keeps the pkg-names guard's own
		// examined-nothing rule quiet; this subtest is about db pairing.
		root := initFixture(t, map[string][]byte{"src/p.go": []byte("package p\n"), "src/p_test.go": []byte("package p\n")}, nil)
		code, out, err := runCobra(t, root, "--json")
		env := decodeEnvelope(t, out)
		if code != 0 {
			t.Fatalf("exit=%d want=0 err=%v stage=%+v", code, err, env.Stages[0])
		}
		g := guardByName(t, env, "db-release-pairing")
		if g.Status != "pass" || g.ExitCode != 0 || g.Files != 1 {
			t.Fatalf("guard=%+v", g)
		}
	})
	t.Run("git grep exit 1 (zero matches) stays a pass", func(t *testing.T) {
		root := initFixture(t, map[string][]byte{"src/p_test.go": []byte("package p\n")}, nil)
		r := runDbPairFakeListing(root, "", []string{"src/p_test.go"})
		if r.Status != "pass" || r.Violations != 0 {
			t.Fatalf("guard=%+v", r)
		}
	})
}

func fakeInvoke(root string, out []byte, err error) static.InvokeFunc {
	return func(cwd string, env []string, args ...string) ([]byte, error) {
		_ = cwd
		_ = root
		_ = env
		_ = args
		return out, err
	}
}

func runDbPairFake(root string, err error) static.Result {
	return static.RunDbPair(root, nil, fakeInvoke(root, nil, err))
}

func runDbPairFakeListing(root, listing string, paths []string) static.Result {
	return static.RunDbPair(root, paths, fakeInvoke(root, []byte(listing), nil))
}

// TestDbPairingZeroTrackedTestsFailsClosed runs the REAL command code
// path over a fixture with no tracked *_test.go at all: the db guard
// examines nothing, which the verify runner must convert to exit 3
// ("examined no applicable inputs"), never a vacuous pass.
func TestDbPairingZeroTrackedTestsFailsClosed(t *testing.T) {
	root := initFixtureOpt(t, goFiles(3), nil, false)
	code, out, err := runCobra(t, root, "--json")
	if code != 3 {
		t.Fatalf("exit=%d want=3 err=%v", code, err)
	}
	env := decodeEnvelope(t, out)
	g := guardByName(t, env, "db-release-pairing")
	if g.Status != "error" || g.ExitCode != 3 || !strings.Contains(g.Excerpt, "db-release-pairing examined no applicable inputs") || g.Files != 0 {
		t.Fatalf("guard=%+v", g)
	}
	if env.Stages[0].ExitCode != 3 || env.OK {
		t.Fatalf("stage=%+v", env.Stages[0])
	}
}
