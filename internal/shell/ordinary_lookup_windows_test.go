//go:build windows

package shell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

func TestOrdinaryCommandLookup_UsesInterpreterPATHAndPATHEXT_Windows(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())

	dir := t.TempDir()
	// Exclude the executable directory from the process environment so the
	// process-environment lookup mutant cannot accidentally find fakecap.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	fake := filepath.Join(dir, "fakecap.exe")
	if err := os.WriteFile(fake, []byte("synthetic executable"), 0o600); err != nil {
		t.Fatalf("write synthetic executable: %v", err)
	}

	var stderr bytes.Buffer
	err := Run(t.Context(), RunOptions{
		Command: "fakecap",
		Cwd:     t.TempDir(),
		Env:     []string{"PATH=" + dir, "PATHEXT=.EXE"},
		Stderr:  &stderr,
	})

	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("error = %T (%v), want *os.PathError from starting resolved fake executable", err, err)
	}
	if pathErr.Path != fake {
		t.Fatalf("PathError.Path = %q, want resolved fake executable %q", pathErr.Path, fake)
	}
	if exitStatus, ok := errors.AsType[interp.ExitStatus](err); ok && exitStatus == interp.ExitStatus(127) {
		t.Fatalf("error = %v, want process-start failure rather than exit status 127", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty stderr for *os.PathError process-start failure", stderr.String())
	}
}
