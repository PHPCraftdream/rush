package cahgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// SnapshotPath is the live acquired manifest; refresh rewrites it with the generated files.
const SnapshotPath = "internal/cahgen/testdata/manifest.json"

// RenderSnapshot is the canonical snapshot encoding: 2-space JSON plus trailing newline.
func RenderSnapshot(m Manifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// CheckCommitted verifies that the committed snapshot is canonical and that the
// committed generated files are exactly Transform of it.
func CheckCommitted(read func(string) ([]byte, error), root string) error {
	raw, err := read(filepath.Join(root, filepath.FromSlash(SnapshotPath)))
	if err != nil {
		return fmt.Errorf("%s: %w", SnapshotPath, err)
	}
	m, err := Decode(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", SnapshotPath, err)
	}
	canon, err := RenderSnapshot(m)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canon) {
		return fmt.Errorf("%s is not the canonical snapshot rendering", SnapshotPath)
	}
	out, err := Transform(m, Overrides())
	if err != nil {
		return fmt.Errorf("%s: %w", SnapshotPath, err)
	}
	for _, f := range []struct {
		path string
		want []byte
	}{{CmdPath, out.Cmd}, {SpecsPath, out.Specs}} {
		got, err := read(filepath.Join(root, filepath.FromSlash(f.path)))
		if err != nil {
			return fmt.Errorf("%s: %w", f.path, err)
		}
		if !bytes.Equal(got, f.want) {
			return fmt.Errorf("%s drifts from %s (cah %s)", f.path, SnapshotPath, m.Version)
		}
	}
	return nil
}
