package cahgen

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureOutput(t *testing.T) Output {
	t.Helper()
	out, e := Transform(fixture(t), Overrides())
	if e != nil {
		t.Fatal(e)
	}
	return out
}

// copyNames lets negative cases mutate Names without touching the shared output.
func copyNames(out Output) Output {
	n := map[string]Name{}
	for k, v := range out.Names {
		n[k] = v
	}
	out.Names = n
	return out
}

// Revert-check: CheckInvariants rules (Transform output) hold for the frozen catalog.
func TestInvariantsFixture(t *testing.T) {
	out := fixtureOutput(t)
	if len(out.Collisions) != 1 || out.Collisions[0].Code != "hl" {
		t.Fatal(out.Collisions)
	}
	if e := CheckInvariants(out); e != nil {
		t.Fatal(e)
	}
}

// Revert-check: CheckInvariants slot-chain, effort, uniqueness and collision checks are not vacuous.
func TestInvariantsNegative(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Output)
		want   string
	}{
		"swapped top and slot1": {func(o *Output) {
			o.Names["ox"], o.Names["o1x"] = o.Names["o1x"], o.Names["ox"]
		}, "slot chain"},
		"codex slot swap": {func(o *Output) {
			o.Names["ls"], o.Names["ls1"] = o.Names["ls1"], o.Names["ls"]
		}, "slot chain"},
		"missing effort": {func(o *Output) {
			n := o.Names["s2xx"]
			n.Effort = "ultra"
			o.Names["s2xx"] = n
		}, "not accepted"},
		"unknown slug": {func(o *Output) {
			n := o.Names["ox"]
			n.Slug = "cli-missing"
			o.Names["ox"] = n
		}, "no model spec"},
		"duplicate target": {func(o *Output) { o.Names["ox1"] = o.Names["ox"] }, "both map"},
		"codex wins collision": {func(o *Output) {
			c := o.Collisions[0]
			c.Winner, c.Loser = c.Loser, c.Winner
			o.Collisions = []Collision{c}
		}, "Claude must win"},
		"collision code not claude": {func(o *Output) { o.Names["hl"] = o.Collisions[0].Loser }, "Claude winner"},
		"collision note missing":    {func(o *Output) { o.Notes = nil }, "missing note"},
	} {
		t.Run(name, func(t *testing.T) {
			out := copyNames(fixtureOutput(t))
			tc.mutate(&out)
			e := CheckInvariants(out)
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", e, tc.want)
			}
		})
	}
}

// Revert-check: modelVersion/versionCmp ordering used by the slot-chain invariant.
func TestModelVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		hi, lo string
	}{
		{"claude-opus-5-5", "claude-opus-5"},
		{"claude-opus-5", "claude-opus-4-8"},
		{"gpt-6.1-sol", "gpt-6-sol"},
		{"gpt-6-sol", "gpt-5.6-sol"},
	} {
		if versionCmp(modelVersion(tc.hi), modelVersion(tc.lo)) <= 0 {
			t.Fatalf("%s not newer than %s", tc.hi, tc.lo)
		}
	}
	if v := modelVersion("gpt-6.1-sol"); len(v) != 2 || v[0] != 6 || v[1] != 1 {
		t.Fatal(v)
	}
}

// Revert-check: Transform's cahNotes (collision, cap skip, uncoded effort) stay user-facing only.
func TestNotes(t *testing.T) {
	out := fixtureOutput(t)
	want := []string{
		"hl means claude-haiku-5-5 low; use local-cli/cli-codex-gpt-6-luna@high for gpt-6-luna high",
		"local-cli/cli-codex-gpt-6-astra@ultra accepted (no cah code)",
		"local-cli/cli-codex-gpt-6-sol@ultra accepted (no cah code)",
		"ul1 omitted: gpt-5.6-luna has no ultra (measured cap)",
	}
	if strings.Join(out.Notes, "\n") != strings.Join(want, "\n") {
		t.Fatal(out.Notes)
	}
	for _, n := range want {
		if !bytes.Contains(out.Cmd, []byte(`"`+n+`"`)) {
			t.Fatalf("cmd lacks note %q", n)
		}
	}
}

// Revert-check: CheckCommitted names the drifting/non-canonical file.
func TestCheckCommittedDrift(t *testing.T) {
	root := t.TempDir()
	seed(t, root)
	if e := CheckCommitted(os.ReadFile, root); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{CmdPath, SpecsPath, SnapshotPath} {
		full := filepath.Join(root, filepath.FromSlash(p))
		orig, e := os.ReadFile(full)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(full, append(append([]byte(nil), orig...), '\n', '\n'), 0o644); e != nil {
			t.Fatal(e)
		}
		if e = CheckCommitted(os.ReadFile, root); e == nil || !strings.Contains(e.Error(), p) {
			t.Fatalf("%s: got %v", p, e)
		}
		if e = os.WriteFile(full, orig, 0o644); e != nil {
			t.Fatal(e)
		}
	}
}

// Revert-check: Run check mode compares the snapshot as well as both generated files.
func TestCheckModeSeesSnapshotDrift(t *testing.T) {
	r, s := acquisition(t)
	w := &fakeWriter{files: map[string][]byte{}}
	d := Dependencies{Runner: r, Source: s, Writer: w, Env: func(string) string { return "" }}
	if c, e := Run(context.Background(), Options{Mode: "refresh"}, d); c != 0 || e != nil {
		t.Fatal(c, e)
	}
	w.files[filepath.FromSlash(SnapshotPath)] = []byte("{}")
	if c, e := Run(context.Background(), Options{Mode: "check"}, d); c != 1 || e != nil {
		t.Fatal("snapshot drift unreported", c, e)
	}
}
