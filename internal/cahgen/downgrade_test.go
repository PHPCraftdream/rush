package cahgen

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Revert-check: removing the downgrade guard in Run makes the older cases write/report drift.
func TestSemverLess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		less bool
	}{
		{"0.15.0", "0.16.0", true},
		{"0.16.0", "0.15.0", false},
		{"0.16.0", "0.16.0", false},
		{"0.9.0", "0.10.0", true},
		{"1.0.0", "0.99.99", false},
		{"0.16.0-rc.1", "0.16.0", true},
		{"0.16.0", "0.16.0-rc.1", false},
		{"0.16.0-rc.1", "0.16.0-rc.2", true},
		{"0.16.0-rc.2", "0.16.0-rc.10", true},
		{"0.16.0-1", "0.16.0-rc", true},
		{"v0.15.0", "0.16.0+build", true},
		{"junk", "0.16.0", false},
		{"0.16.0", "junk", false},
		{"0.16", "0.16.1", false},
	} {
		if got := semverLess(tc.a, tc.b); got != tc.less {
			t.Fatalf("semverLess(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// Revert-check: Run's downgrade guard message and no-write behaviour for an older acquired cah.
func TestDowngradeGuard(t *testing.T) {
	cmdPath := filepath.Join("root", filepath.FromSlash(CmdPath))
	for _, tc := range []struct {
		name, committed       string
		mode                  string
		strict                bool
		wantCode              int
		wantErr, wantWrite    bool
		wantWarning, wantKept bool
	}{
		{name: "older refresh", committed: "0.17.0", mode: "refresh", wantKept: true, wantWarning: true},
		{name: "older refresh strict", committed: "0.17.0", mode: "refresh", strict: true, wantCode: 1, wantErr: true, wantKept: true, wantWarning: true},
		{name: "older check", committed: "0.17.0", mode: "check", wantKept: true, wantWarning: true},
		{name: "older check strict", committed: "0.17.0", mode: "check", strict: true, wantCode: 1, wantErr: true, wantKept: true, wantWarning: true},
		{name: "newer refresh", committed: "0.15.0", mode: "refresh", wantWrite: true},
		{name: "newer check reports drift", committed: "0.15.0", mode: "check", wantCode: 1},
		{name: "equal refresh", committed: "0.16.0", mode: "refresh", wantKept: true},
		{name: "equal check", committed: "0.16.0", mode: "check", wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := acquisition(t)
			w := &fakeWriter{files: map[string][]byte{}}
			env := func(string) string { return "" }
			if code, err := Run(context.Background(), Options{Root: "root", Mode: "refresh"}, Dependencies{Runner: r, Source: s, Writer: w, Env: env}); code != 0 || err != nil {
				t.Fatal(code, err)
			}
			w.files[cmdPath] = bytes.Replace(w.files[cmdPath], []byte(`cahVersion = "0.16.0"`), []byte(`cahVersion = "`+tc.committed+`"`), 1)
			before := map[string]string{}
			for k, v := range w.files {
				before[k] = string(v)
			}
			w.stage = 0
			var logs []string
			code, err := Run(context.Background(), Options{Root: "root", Mode: tc.mode, Strict: tc.strict}, Dependencies{Runner: r, Source: s, Writer: w, Env: env, Log: func(m string) { logs = append(logs, m) }})
			if code != tc.wantCode || (err != nil) != tc.wantErr {
				t.Fatal(code, err)
			}
			warning := "warning: npm returned 0.16.0, older than committed " + tc.committed + " (npm min-release-age?) — keeping committed files"
			if strings.Contains(strings.Join(logs, "\n"), warning) != tc.wantWarning {
				t.Fatal(logs)
			}
			if tc.wantKept && (w.stage != 0 || string(w.files[cmdPath]) != before[cmdPath]) {
				t.Fatal("committed files modified", w.stage)
			}
			if tc.wantWrite && !bytes.Contains(w.files[cmdPath], []byte(`cahVersion = "0.16.0"`)) {
				t.Fatal("newer acquisition not written")
			}
		})
	}
}
