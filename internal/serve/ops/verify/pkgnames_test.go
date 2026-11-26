package verify

// Command-level port of .githooks/check_pkg_names_selftest.sh cases 1-7
// (case 8, the real repository, is TestTrackedCheckoutResultsAreSevenGuardOnly),
// plus the broken-input contract the shell collapses onto exit 1 and the
// native port keeps at exit 3. All cases run the real command code path.

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeEnvelope(t *testing.T, out []byte) Envelope {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return env
}

func guardByName(t *testing.T, env Envelope, name string) Guard {
	t.Helper()
	for _, g := range env.Stages[0].Guards {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("guard %s missing from envelope: %+v", name, env.Stages[0])
	return Guard{}
}

func TestPkgNamesSelftestCases(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		lists   map[string]string
		exit    int
		excerpt string
	}{
		{
			name: "unique package names pass",
			files: map[string][]byte{
				"a/x.go": []byte("package alpha\n"),
				"b/y.go": []byte("package beta\n"),
			},
			exit: 0,
		},
		{
			// ALLOWLIST-COVERAGE PIN: this "duplicate package names
			// fail" case runs with an EMPTY allowlist, so it proves the
			// duplicate would fail if the allowlist were absent;
			// "allowlisted collision at its recorded count passes" below
			// proves the allowlist actually matters (same fixture, same
			// counts, pass only because "dup 2" is recorded).
			name: "duplicate package names fail, both directories named",
			files: map[string][]byte{
				"a/x.go": []byte("package dup\n"),
				"b/y.go": []byte("package dup\n"),
			},
			exit:    1,
			excerpt: "DUPLICATE PACKAGE NAME: \"dup\" is declared by 2 directories: a b",
		},
		{
			name: "main packages do not collide",
			files: map[string][]byte{
				"a/x.go": []byte("package main\n"),
				"b/y.go": []byte("package main\n"),
			},
			exit: 0,
		},
		{
			name: "test-file-only directories are ignored",
			files: map[string][]byte{
				"a/x.go":      []byte("package dup\n"),
				"b/y_test.go": []byte("package dup\n"),
			},
			exit: 0,
		},
		{
			name: "testdata packages are ignored",
			files: map[string][]byte{
				"a/x.go":          []byte("package dup\n"),
				"a/testdata/z.go": []byte("package dup\n"),
			},
			exit: 0,
		},
		{
			name: "allowlisted collision at its recorded count passes",
			files: map[string][]byte{
				"a/x.go": []byte("package dup\n"),
				"b/y.go": []byte("package dup\n"),
			},
			lists: map[string]string{"pkg_names_allowlist.txt": "dup 2\n"},
			exit:  0,
		},
		{
			name: "allowlisted collision grown by one dir fails",
			files: map[string][]byte{
				"a/x.go": []byte("package dup\n"),
				"b/y.go": []byte("package dup\n"),
				"c/z.go": []byte("package dup\n"),
			},
			lists:   map[string]string{"pkg_names_allowlist.txt": "dup 2\n"},
			exit:    1,
			excerpt: "\"dup\" is now declared by 3 directories (allowlist records 2)",
		},
		{
			name: "allowlisted name now unique fails with cleanup",
			files: map[string][]byte{
				"a/x.go": []byte("package dup\n"),
			},
			lists:   map[string]string{"pkg_names_allowlist.txt": "dup 2\n"},
			exit:    1,
			excerpt: "RATCHET CLEANUP: \"dup\" is now unique",
		},
		{
			name: "malformed allowlist is a broken input",
			files: map[string][]byte{
				"a/x.go": []byte("package alpha\n"),
			},
			lists: map[string]string{"pkg_names_allowlist.txt": "broken\n"},
			exit:  3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string][]byte{}
			for p, c := range tc.files {
				body[p] = c
			}
			for name, content := range tc.lists {
				body[".githooks/"+name] = []byte(content)
			}
			root := initFixture(t, body, nil)
			code, out, err := runCobra(t, root, "--json")
			if code != tc.exit {
				t.Fatalf("exit=%d want=%d err=%v", code, tc.exit, err)
			}
			env := decodeEnvelope(t, out)
			g := guardByName(t, env, "pkg-names")
			wantStatus := "pass"
			switch tc.exit {
			case 1:
				wantStatus = "fail"
			case 3:
				wantStatus = "error"
			}
			if g.Status != wantStatus || g.ExitCode != tc.exit {
				t.Fatalf("status=%s exit=%d want %s/%d", g.Status, g.ExitCode, wantStatus, tc.exit)
			}
			if tc.excerpt != "" && !strings.Contains(g.Excerpt, tc.excerpt) {
				t.Fatalf("excerpt missing %q: %q", tc.excerpt, g.Excerpt)
			}
			if tc.exit == 0 && g.Files == 0 {
				t.Fatalf("passing guard examined nothing: %+v", g)
			}
			if tc.name == "duplicate package names fail, both directories named" {
				if !strings.Contains(g.Excerpt, " a ") || !strings.Contains(g.Excerpt, " b") {
					t.Fatalf("both directories must be named: %q", g.Excerpt)
				}
			}
			if tc.name == "allowlisted name now unique fails with cleanup" && !strings.Contains(g.Excerpt, "remove it from") {
				t.Fatalf("cleanup must ask for removal: %q", g.Excerpt)
			}
		})
	}
}

// Package-name parity pin (deliberate native divergence, see the
// comment in static/pkgnames.go): the shell's field extract yields the
// token "main;" for a `package main;` line and therefore does NOT
// exempt it, while the native regex extracts the identifier "main" and
// keeps the shell's INTENT (the exemption is for package main entry
// points). Two directories declaring `package main;` must pass.
func TestPkgNamesPackageMainWithSemicolonIsExempted(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string][]byte
		lists   map[string]string
		exit    int
		excerpt string
	}{
		{
			name: "two dirs with `package main;` pass (native keeps the exemption)",
			files: map[string][]byte{
				"a/x.go": []byte("package main;\n"),
				"b/y.go": []byte("package main;\n"),
			},
			exit: 0,
		},
		{
			name: "contrast: two dirs with `package dup;` are NOT exempted",
			files: map[string][]byte{
				"a/x.go": []byte("package dup;\n"),
				"b/y.go": []byte("package dup;\n"),
			},
			exit:    1,
			excerpt: "DUPLICATE PACKAGE NAME: \"dup\" is declared by 2 directories",
		},
		{
			name: "contrast with allowlist at recorded count passes",
			files: map[string][]byte{
				"a/x.go": []byte("package dup;\n"),
				"b/y.go": []byte("package dup;\n"),
			},
			lists: map[string]string{"pkg_names_allowlist.txt": "dup 2\n"},
			exit:  0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string][]byte{}
			for p, c := range tc.files {
				body[p] = c
			}
			for name, content := range tc.lists {
				body[".githooks/"+name] = []byte(content)
			}
			root := initFixture(t, body, nil)
			code, out, err := runCobra(t, root, "--json")
			if code != tc.exit {
				t.Fatalf("exit=%d want=%d err=%v", code, tc.exit, err)
			}
			g := guardByName(t, decodeEnvelope(t, out), "pkg-names")
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
		})
	}
}
