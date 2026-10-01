//go:build !windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

const sigpipeHelperEnv = "RUSH_SIGPIPE_HELPER"

// TestBrokenPipeHelperProcess is the child of TestKeepBrokenPipeAsError: its
// stdout is a pipe whose reader is gone. Mode "guard" installs the guard first.
// On EPIPE the child writes the marker (what the exit flush's caller would do:
// run the hook, shut down) and exits 1; if the process survives without the
// error it exits 3.
func TestBrokenPipeHelperProcess(t *testing.T) {
	mode := os.Getenv(sigpipeHelperEnv)
	if mode == "" {
		t.Skip("helper of TestKeepBrokenPipeAsError")
	}
	if mode == "guard" {
		defer keepBrokenPipeAsError()()
	}
	_, err := os.Stdout.WriteString(`{"exit_reason":"end_turn"}` + "\n")
	if !errors.Is(err, syscall.EPIPE) {
		os.Exit(3)
	}
	if werr := os.WriteFile(os.Getenv(sigpipeHelperEnv+"_MARKER"), []byte("hook ran"), 0o600); werr != nil {
		os.Exit(4)
	}
	os.Exit(1)
}

func runBrokenPipeHelper(t *testing.T, mode string) (marker string, err error) {
	t.Helper()
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	require.NoError(t, r.Close()) // no reader: every write raises SIGPIPE
	t.Cleanup(func() { _ = w.Close() })
	marker = filepath.Join(t.TempDir(), "marker")
	cmd := platform.Command(t.Context(), os.Args[0], "-test.run=^TestBrokenPipeHelperProcess$")
	cmd.Env = append(os.Environ(), sigpipeHelperEnv+"="+mode, sigpipeHelperEnv+"_MARKER="+marker)
	cmd.Stdout = w
	return marker, cmd.Run()
}

// R8C-7: with the guard, a write to a closed stdout returns EPIPE and the
// process carries on (marker written, status 1) instead of dying by SIGPIPE
// before --on-finish and Shutdown. The control run shows the platform default
// this guards against, so the test cannot pass vacuously.
//
// Revert-check: an empty keepBrokenPipeAsError turns the guard case into the
// control (signal: broken pipe, no marker).
func TestKeepBrokenPipeAsError(t *testing.T) {
	t.Run("guard installed", func(t *testing.T) {
		marker, err := runBrokenPipeHelper(t, "guard")
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 1, exitErr.ExitCode(), "the helper's own EPIPE exit, not a signal death: %v", err)
		_, statErr := os.Stat(marker)
		require.NoError(t, statErr, "the code after the failed write ran")
	})
	t.Run("control without the guard", func(t *testing.T) {
		marker, err := runBrokenPipeHelper(t, "plain")
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		ws, ok := exitErr.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		if !ws.Signaled() {
			t.Skip("this environment ignores SIGPIPE; the default cannot be shown")
		}
		require.Equal(t, syscall.SIGPIPE, ws.Signal(), "the default is death by SIGPIPE, got %v", err)
		_, statErr := os.Stat(marker)
		require.True(t, os.IsNotExist(statErr))
	})
}
