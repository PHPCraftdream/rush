//go:build !windows

package shell

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestXargsBuiltin_MaxArgs exercises the Go coreutils xargs handler end to
// end. useGoCoreUtils is forced to true because it is a Windows-only default;
// CI on Linux and macOS must exercise the u-root path too. The cleanup
// restores the original value.
func TestXargsBuiltin_MaxArgs(t *testing.T) {
	if _, err := exec.LookPath("echo"); err != nil {
		t.Skip("host echo binary required as the xargs COMMAND")
	}
	if coreUtilsExecHandler == nil {
		t.Skip("Go coreutils handler unavailable on this platform")
	}
	orig := useGoCoreUtils
	useGoCoreUtils = true
	t.Cleanup(func() { useGoCoreUtils = orig })

	t.Run("n1", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := Run(ctx, RunOptions{
			Command: "printf 'a\\nb\\n' | xargs -n1 echo",
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
		require.NoError(t, err)
		require.Equal(t, "a\nb\n", stdout.String())
	})

	t.Run("n1 in pipeline goroutine", func(t *testing.T) {
		// xargs is the LEFT stage of the inner pipe of this 3-command
		// pipeline, so it runs in the wg.Go goroutine
		// (interp/runner.go:498-506) — the exact shape from the bug report.
		// cat is a u-root coreutil (moreinterp coreutils.go
		// commandBuilders), so no host cat is needed.
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := Run(ctx, RunOptions{
			Command: "printf 'a\\nb\\n' | xargs -n1 echo | cat",
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
		require.NoError(t, err)
		require.Equal(t, "a\nb\n", stdout.String())
	})

	t.Run("n0 invalid must not hang", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := Run(ctx, RunOptions{
			Command: "printf 'a\\nb\\n' | xargs -n0 echo",
			Cwd:     t.TempDir(),
			Env:     os.Environ(),
			Stdout:  &stdout,
			Stderr:  &stderr,
		})
		require.Error(t, err)
		require.Equal(t, 1, ExitCode(err))
		require.Contains(t, stderr.String(), "xargs: invalid number for -n option")
	})
}
