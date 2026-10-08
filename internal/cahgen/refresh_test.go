package cahgen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seed copies the committed generated files and snapshot from the repo into root.
func seed(t *testing.T, root string) {
	t.Helper()
	for _, p := range []string{CmdPath, SpecsPath, SnapshotPath} {
		b, e := os.ReadFile(filepath.Join("../..", filepath.FromSlash(p)))
		if e != nil {
			t.Fatal(e)
		}
		dst := filepath.Join(root, filepath.FromSlash(p))
		if e = os.MkdirAll(filepath.Dir(dst), 0o755); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(dst, b, 0o644); e != nil {
			t.Fatal(e)
		}
	}
}

// Revert-check: Run's snapshot publication (SnapshotPath in Publish) is what keeps refresh test-free.
func TestRefreshNewCatalogNeedsNoTestEdits(t *testing.T) {
	// Far-future version so a later real refresh never turns this into a downgrade.
	const newVersion = "99.0.0"
	m := fixture(t)
	m.Version = newVersion
	for i := range m.Claude {
		if m.Claude[i].Model == "claude-opus-5-5" {
			m.Claude[i].Model = "claude-opus-6"
		}
	}
	root := t.TempDir()
	seed(t, root)
	r, s := acquisitionOf(t, m, newVersion)
	code, err := Run(context.Background(), Options{Root: root, Mode: "refresh"}, Dependencies{Runner: r, Source: s, Writer: OSWriter{}, Env: func(string) string { return "" }})
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if e := CheckCommitted(os.ReadFile, root); e != nil {
		t.Fatal(e)
	}
	out, err := Transform(m, Overrides())
	if err != nil {
		t.Fatal(err)
	}
	if e := CheckInvariants(out); e != nil {
		t.Fatal(e)
	}
	cmd, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(CmdPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cmd), "claude-opus-6") || !strings.Contains(string(cmd), `const cahVersion = "`+newVersion+`"`) {
		t.Fatal("refreshed cmd file lacks the new catalog")
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(SnapshotPath)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decode(raw); err != nil || got.Version != newVersion {
		t.Fatal(got.Version, err)
	}
}
