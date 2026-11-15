// Unit tests for the `rush run --worktree` flag validation, stream
// mirroring, session id checks, and mirror flushing.
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newWorktreeFlagTestCmd returns a bare command with the --cwd and
// --data-dir local flags validateRunWorktreeFlags reads.
func newWorktreeFlagTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "run"}
	c.Flags().StringP("cwd", "c", "", "")
	c.Flags().String("data-dir", "", "")
	return c
}

// Revert-check: catches regressions where --worktree/--base/--branch
// validation stops enforcing conflicts and slug rules.
func TestValidateRunWorktreeFlags(t *testing.T) {
	tests := []struct {
		name      string
		slug      string
		base      string
		branch    string
		cwd       string
		dataDir   string
		wantErrBy string // Substring of the expected error; "" = no error.
	}{
		{name: "all empty ok"},
		{name: "slug base branch ok", slug: "task-a", base: "main", branch: "feat/x"},
		{name: "slug only ok", slug: "task-a"},
		{name: "cwd conflict", slug: "task-a", cwd: "/tmp/x", wantErrBy: "--cwd"},
		{name: "data-dir conflict", slug: "task-a", dataDir: "/tmp/d", wantErrBy: "--data-dir"},
		{name: "base without slug", base: "main", wantErrBy: "require --worktree"},
		{name: "branch without slug", branch: "feat/x", wantErrBy: "require --worktree"},
		{name: "base and branch without slug", base: "main", branch: "feat/x", wantErrBy: "require --worktree"},
		{name: "invalid slug dotdot", slug: "..", wantErrBy: "worktree slug"},
		{name: "invalid slug uppercase", slug: "A", wantErrBy: "worktree slug"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newWorktreeFlagTestCmd()
			require.NoError(t, cmd.Flags().Set("cwd", tc.cwd))
			require.NoError(t, cmd.Flags().Set("data-dir", tc.dataDir))
			err := validateRunWorktreeFlags(cmd, tc.slug, tc.base, tc.branch)
			if tc.wantErrBy == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErrBy)
		})
	}
}

// Revert-check: catches regressions where the mirror tail drops bytes it
// must retain or keeps more than its cap.
func TestTailBuffer(t *testing.T) {
	t.Run("under cap verbatim", func(t *testing.T) {
		tb := newTailBuffer(16)
		n, err := tb.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.Equal(t, []byte("hello"), tb.Bytes())
	})
	t.Run("beyond cap keeps last bytes", func(t *testing.T) {
		tb := newTailBuffer(4)
		_, err := tb.Write([]byte("abcdefgh"))
		require.NoError(t, err)
		require.Equal(t, []byte("efgh"), tb.Bytes())
	})
	t.Run("zero length write ok", func(t *testing.T) {
		tb := newTailBuffer(4)
		n, err := tb.Write(nil)
		require.NoError(t, err)
		require.Equal(t, 0, n)
		require.Empty(t, tb.Bytes())
	})
}

// Revert-check: catches regressions where the mirror stops passing
// through to the original streams, loses the tail, or fails to restore
// os.Stdout/os.Stderr.
func TestStreamMirror(t *testing.T) {
	outOrig, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	defer outOrig.Close()
	errOrig, err := os.CreateTemp(t.TempDir(), "err")
	require.NoError(t, err)
	defer errOrig.Close()

	prevStdout, prevStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outOrig, errOrig
	defer func() {
		os.Stdout, os.Stderr = prevStdout, prevStderr
	}()

	m, err := startStreamMirror()
	require.NoError(t, err)

	chunk := strings.Repeat("o", streamMirrorCapBytes+1024)
	_, err = os.Stdout.WriteString(chunk)
	require.NoError(t, err)
	_, err = os.Stderr.WriteString("stderr-marker")
	require.NoError(t, err)

	outTail, errTail, stopErr := m.stop()
	require.NoError(t, stopErr)

	// Original files contain everything written (passthrough).
	require.NoError(t, outOrig.Sync())
	require.NoError(t, errOrig.Sync())
	passthrough, err := os.ReadFile(outOrig.Name())
	require.NoError(t, err)
	require.Len(t, passthrough, len(chunk))
	errPassthrough, err := os.ReadFile(errOrig.Name())
	require.NoError(t, err)
	require.Equal(t, "stderr-marker", string(errPassthrough))

	// Tails contain the marker and respect the cap.
	require.LessOrEqual(t, len(outTail), streamMirrorCapBytes)
	require.LessOrEqual(t, len(errTail), streamMirrorCapBytes)
	require.Contains(t, string(errTail), "stderr-marker")
	require.Equal(t, chunk[len(chunk)-streamMirrorCapBytes:], string(outTail))

	// os.Stdout/os.Stderr restored to the swapped-in files.
	require.Same(t, outOrig, os.Stdout)
	require.Same(t, errOrig, os.Stderr)
}

// Revert-check: catches regressions where a session id containing
// separators or dot segments can escape .rush/stdin.
func TestValidateMirrorSessionID(t *testing.T) {
	for _, id := range []string{"task-a", "pr-42"} {
		require.NoError(t, validateMirrorSessionID(id), id)
	}
	for _, id := range []string{"", "a/b", `a\b`, ".", "..", "a/b/c"} {
		require.Error(t, validateMirrorSessionID(id), id)
	}
}

// Revert-check: catches regressions where mirrored output is not written
// to .rush/stdin, written with wrong content, or not overwritten on a
// second run with the same session id.
func TestFlushRunWorktreeMirror(t *testing.T) {
	mainRoot := t.TempDir()
	err := flushRunWorktreeMirror(mainRoot, "task-a",
		[]byte("out one"), []byte("err one"))
	require.NoError(t, err)
	out, err := os.ReadFile(filepath.Join(mainRoot, ".rush", "stdin", "task-a.out"))
	require.NoError(t, err)
	require.Equal(t, "out one", string(out))
	errF, err := os.ReadFile(filepath.Join(mainRoot, ".rush", "stdin", "task-a.err"))
	require.NoError(t, err)
	require.Equal(t, "err one", string(errF))

	err = flushRunWorktreeMirror(mainRoot, "task-a", []byte("two"), nil)
	require.NoError(t, err)
	out, err = os.ReadFile(filepath.Join(mainRoot, ".rush", "stdin", "task-a.out"))
	require.NoError(t, err)
	require.Equal(t, "two", string(out))
	errF, err = os.ReadFile(filepath.Join(mainRoot, ".rush", "stdin", "task-a.err"))
	require.NoError(t, err)
	require.Empty(t, string(errF))
}
