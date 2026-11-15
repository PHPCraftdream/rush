package cahgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
)

// RefreshBuild launches the generator with the real environment, not pnpm's synthetic CI.
func RefreshBuild(ctx context.Context, root string, stdout, stderr io.Writer) error {
	return refreshBuild(ctx, root, stdout, stderr, os.Getenv, func(cmd *exec.Cmd) error { return cmd.Run() })
}

func refreshBuild(ctx context.Context, root string, stdout, stderr io.Writer, env func(string) string, launch func(*exec.Cmd) error) error {
	cmd := platform.Command(ctx, "go", "run", "./internal/kit/dev/cahsync", "-mode", "refresh")
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, stdout, stderr
	if err := launch(cmd); err != nil {
		_, err = failure(env("CI") == "true", fmt.Errorf("generator launch: %w", err), func(s string) { fmt.Fprintln(stderr, s) })
		return err
	}
	return nil
}

func failure(strict bool, err error, log func(string)) (int, error) {
	if _, broken := err.(*RollbackError); strict || broken {
		return 1, err
	}
	log("warning: cahsync failed; committed outputs untouched: " + err.Error())
	return 0, nil
}

// validateAcceptedEfforts checks raw aliases before collision/cap filtering.
func validateAcceptedEfforts(m Manifest, overrides map[string]Override) error {
	if m.AcceptedEfforts == nil {
		return nil
	}
	models := map[string]bool{}
	for _, e := range m.Codex {
		models[e.Model] = true
	}
	if len(models) != len(m.AcceptedEfforts) {
		return fmt.Errorf("acceptedEfforts model set mismatch")
	}
	for model, efforts := range m.AcceptedEfforts {
		if !models[model] || !codexRE.MatchString(model) || len(efforts) == 0 {
			return fmt.Errorf("invalid acceptedEfforts model %s", model)
		}
		for i, effort := range efforts {
			if rank(effort) < 0 || i > 0 && rank(efforts[i-1]) >= rank(effort) {
				return fmt.Errorf("invalid acceptedEfforts set %s", model)
			}
		}
		if o, ok := overrides[model]; ok && len(o.EffortCaps) > 0 && !slices.Equal(efforts, o.EffortCaps) {
			return fmt.Errorf("acceptedEfforts measured cap mismatch %s", model)
		}
	}
	for _, e := range m.Codex {
		if !slices.Contains(m.AcceptedEfforts[e.Model], e.Effort) {
			return fmt.Errorf("acceptedEfforts alias mismatch %s", e.Name)
		}
	}
	return nil
}

// SnapshotPath is the live acquired manifest; refresh rewrites it with the generated files.
const SnapshotPath = "internal/kit/dev/cahgen/testdata/manifest.json"

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
