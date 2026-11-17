// Unit tests for `rush run --worktree` flag validation, session id checks,
// and mirror flushing.
package run

import (
	"os"
	"path/filepath"
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
