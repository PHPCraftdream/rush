package cahgen

import (
	"context"
	"path"
	"strings"
	"testing"
)

// Revert-check: Codes must match effort, family and positive canonical slots.
func TestCodeScheme(t *testing.T) {
	for _, tc := range []struct {
		family              int
		name, model, effort string
		valid               bool
	}{
		{0, "f12xx", "claude-fable-5", "max", true},
		{1, "xxs12", "gpt-6-sol", "max", true},
		{0, "hm", "claude-haiku-5-5", "medium", true},
		{0, "ha", "claude-haiku-5-5", "high", false},
		{0, "hl1", "claude-haiku-5-5", "low", false},
		{0, "h0l", "claude-haiku-5-5", "low", false},
		{0, "h01l", "claude-haiku-5-5", "low", false},
		{0, "hl", "claude-haiku-5-5", "high", false},
		{0, "sl", "claude-haiku-5-5", "low", false},
		{0, "hu", "claude-haiku-5-5", "ultra", false},
		{1, "mh", "gpt-6-sol", "medium", false},
		{1, "sl", "gpt-6-luna", "low", false},
		{1, "ls0", "gpt-6-sol", "low", false},
		{1, "ls01", "gpt-6-sol", "low", false},
		{1, "l1s", "gpt-6-sol", "low", false},
		{1, "ls", "gpt-6-luna", "low", false},
		{1, "ls", "gpt-6-sol", "high", false},
		{1, "lsjunk", "gpt-6-sol", "low", false},
	} {
		e := Entry{Name: tc.name, Model: tc.model, Effort: tc.effort}
		if validName(e, tc.family) != tc.valid {
			t.Fatalf("%+v", tc)
		}
		if !tc.valid {
			m := fixture(t)
			if tc.family == 0 {
				m.Claude[0].Name = tc.name
				m.Claude[0].Model = tc.model
				m.Claude[0].Effort = tc.effort
			} else {
				m.Codex[0].Name = tc.name
				m.Codex[0].Model = tc.model
				m.Codex[0].Effort = tc.effort
			}
			if _, err := Transform(m, Overrides()); err == nil {
				t.Fatalf("accepted %+v", tc)
			}
		}
	}
}

// Revert-check: Unknown collisions fail independently of code syntax validation.
func TestCollisionRules(t *testing.T) {
	prior := Name{Model: "claude-haiku-5-5", Effort: "low"}
	next := Name{Model: "gpt-6-luna", Effort: "high", Slug: "cli-codex-gpt-6-luna"}
	if _, err := resolveCollision("hl", prior, next, collisionRules); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCollision("hl", prior, next, nil); err == nil {
		t.Fatal("missing permission accepted")
	}
	// Exercise the collision resolver directly: do not fabricate invalid schema fixtures.
	if _, err := resolveCollision("second", prior, next, collisionRules); err == nil || !strings.Contains(err.Error(), "unresolved cross-family collision second") {
		t.Fatal(err)
	}
	next.Effort = "low"
	if _, err := resolveCollision("hl", prior, next, collisionRules); err == nil {
		t.Fatal("mismatched permission accepted")
	}
}

// Revert-check: Exact diagnostics and default Codex slugs remain stable.
func TestRequiredDiagnostics(t *testing.T) {
	o := Overrides()
	delete(o, "claude-fable-5-1")
	out, err := Transform(fixture(t), o)
	if err != nil {
		t.Fatal(err)
	}
	contains := func(list []string, want string) {
		t.Helper()
		for _, s := range list {
			if s == want {
				return
			}
		}
		t.Fatalf("missing %q in %v", want, list)
	}
	contains(out.Warnings, "default spec (unmeasured): claude-fable-5-1")
	contains(out.Warnings, "skipped ul1: effort above measured cap")
	contains(out.Info, "codex context fallback 272000 (cah has no contextWindow)")
	if out.Names["ls"].Slug != "cli-codex-gpt-6-1-sol" {
		t.Fatal(out.Names["ls"])
	}
	if strings.Contains(string(out.Specs), "integration chunk") {
		t.Fatal("scaffolding emitted")
	}
}

// Revert-check: Flag conflicts return errors without network or environment reads.
func TestFlagModes(t *testing.T) {
	for _, o := range []Options{
		{Check: true, Offline: true}, {Mode: "refresh", Check: true}, {Mode: "check", Offline: true}, {Mode: "offline", Check: true},
	} {
		r := &fakeRunner{}
		code, err := Run(context.Background(), o, Dependencies{Runner: r, Env: func(string) string { panic("conflict I/O") }})
		if code != 1 || err == nil || len(r.calls) != 0 {
			t.Fatal(o, code, err, r.calls)
		}
	}
	for _, o := range []Options{{Offline: true}, {Mode: "offline", Offline: true}} {
		if code, err := Run(context.Background(), o, Dependencies{Env: func(string) string { panic("offline I/O") }}); code != 0 || err != nil {
			t.Fatal(code, err)
		}
	}
	r, s := acquisition(t)
	w := &fakeWriter{files: map[string][]byte{}}
	if code, err := Run(context.Background(), Options{Check: true}, Dependencies{Runner: r, Source: s, Writer: w}); code != 1 || err != nil || w.stage != 0 {
		t.Fatal(code, err)
	}
}

// Revert-check: Run's per-file "changed: commit it" log, snapshot included, and version log.
func TestSuccessLogs(t *testing.T) {
	r, s := acquisition(t)
	w := &fakeWriter{files: map[string][]byte{}}
	var logs []string
	d := Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }, Log: func(s string) { logs = append(logs, s) }}
	if code, err := Run(context.Background(), Options{}, d); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	joined := strings.Join(logs, "\n")
	for _, p := range []string{CmdPath, SpecsPath, SnapshotPath} {
		if !strings.Contains(strings.ReplaceAll(joined, "\\", "/"), path.Base(p)+" changed: commit it: "+p) {
			t.Fatal(logs)
		}
	}
	if !strings.Contains(joined, "commit them") {
		t.Fatal(logs)
	}
	for _, mode := range []string{"refresh", "check"} {
		logs = nil
		before := w.stage
		if code, err := Run(context.Background(), Options{Mode: mode}, d); code != 0 || err != nil {
			t.Fatal(code, err)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "cah version: 0.16.0") || w.stage != before || strings.Contains(strings.Join(logs, "\n"), "changed: commit it") {
			t.Fatal(logs)
		}
	}
}
