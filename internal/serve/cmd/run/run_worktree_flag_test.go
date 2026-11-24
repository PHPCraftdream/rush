// Unit tests for `rush run --worktree` flag validation, session id checks,
// and mirror flushing.
package run

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	wtree "github.com/PHPCraftdream/rush/internal/serve/cmd/run/wtree"
)

// newWorktreeFlagTestCmd returns a bare command with the --cwd and
// --data-dir local flags validateRunWorktreeFlags reads.
func newWorktreeFlagTestCmd() *cobra.Command {
	c := &cobra.Command{Use: "run"}
	c.Flags().StringP("cwd", "c", "", "")
	c.Flags().String("data-dir", "", "")
	c.Flags().Bool("isolated-store", false, "")
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
		{name: "data-dir allowed", slug: "task-a", dataDir: "/tmp/d"},
		{name: "cwd conflict", slug: "task-a", cwd: "/tmp/x", wantErrBy: "--cwd"},
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

func TestApplyRunWorktreeDataDir(t *testing.T) {
	cmd := newWorktreeFlagTestCmd()
	require.NoError(t, cmd.Flags().Set("isolated-store", "true"))
	require.NoError(t, applyRunWorktreeDataDir(cmd, "/repo", "session-1"))
	got, err := cmd.Flags().GetString("data-dir")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("/repo", ".rush", "agent-dbs", "session-1", ".rush"), got)
}

func TestApplyRunWorktreeDataDirNoFlagsPreservesDefault(t *testing.T) {
	cmd := newWorktreeFlagTestCmd()
	require.NoError(t, applyRunWorktreeDataDir(cmd, "/repo", "session-1"))
	got, err := cmd.Flags().GetString("data-dir")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestApplyRunWorktreeDataDirExplicitWins(t *testing.T) {
	cmd := newWorktreeFlagTestCmd()
	require.NoError(t, cmd.Flags().Set("data-dir", "/explicit/store"))
	require.NoError(t, cmd.Flags().Set("isolated-store", "true"))
	require.NoError(t, applyRunWorktreeDataDir(cmd, "/repo", "session-1"))
	got, err := cmd.Flags().GetString("data-dir")
	require.NoError(t, err)
	require.Equal(t, "/explicit/store", got)
}

func TestApplyRunWorktreeDataDirExplicitSelector(t *testing.T) {
	cmd := newWorktreeFlagTestCmd()
	require.NoError(t, cmd.Flags().Set("data-dir", "/explicit/store"))
	require.NoError(t, cmd.Flags().Set("isolated-store", "false"))
	require.NoError(t, applyRunWorktreeDataDir(cmd, "/repo", "session-1"))
	got, err := cmd.Flags().GetString("data-dir")
	require.NoError(t, err)
	require.Equal(t, "/explicit/store", got)
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

func mustGetwd(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	return cwd
}

func TestFinishRunWorktreeOutputLifecycle(t *testing.T) {
	mainRoot := t.TempDir()
	stdinDir := filepath.Join(mainRoot, ".rush", "stdin")
	require.NoError(t, os.MkdirAll(stdinDir, 0o755))
	prevOut, prevErr := os.Stdout, os.Stderr
	outOrig, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	defer outOrig.Close()
	errOrig, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	defer errOrig.Close()
	os.Stdout, os.Stderr = outOrig, errOrig
	t.Cleanup(func() { os.Stdout, os.Stderr = prevOut, prevErr })
	started := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	start := func(at time.Time, data []byte) *runWorktree {
		mirror, startErr := wtree.StartStreamMirror()
		require.NoError(t, startErr)
		if len(data) > 0 {
			_, writeErr := os.Stdout.Write(data)
			require.NoError(t, writeErr)
			_, writeErr = os.Stderr.Write([]byte("stderr tail"))
			require.NoError(t, writeErr)
		}
		return &runWorktree{sessionID: "task-a", mainRoot: mainRoot, mirror: mirror, prevCwd: mustGetwd(t), startedAt: at}
	}
	first := start(started, []byte(`{"exit_reason":"end_turn"}`))
	first.finish()
	latestPath := filepath.Join(stdinDir, "task-a.out")
	firstOutput, err := os.ReadFile(latestPath)
	require.NoError(t, err)
	require.Equal(t, `{"exit_reason":"end_turn"}`, string(firstOutput))
	firstErr, err := os.ReadFile(filepath.Join(stdinDir, "task-a.err"))
	require.NoError(t, err)
	require.Equal(t, "stderr tail", string(firstErr))

	second := start(started.Add(time.Minute), nil)
	second.finish()
	latest, err := os.ReadFile(latestPath)
	require.NoError(t, err)
	require.Equal(t, firstOutput, latest)
	latestErr, err := os.ReadFile(filepath.Join(stdinDir, "task-a.err"))
	require.NoError(t, err)
	require.Empty(t, latestErr)
	archives, err := filepath.Glob(filepath.Join(stdinDir, "task-a.*.out"))
	require.NoError(t, err)
	require.Empty(t, archives)
	errArchives, err := filepath.Glob(filepath.Join(stdinDir, "task-a.*.err"))
	require.NoError(t, err)
	require.Len(t, errArchives, 1)

	third := start(started.Add(2*time.Minute), []byte(`{"exit_reason":"error","error":"failed"}`))
	third.finish()
	latest, err = os.ReadFile(latestPath)
	require.NoError(t, err)
	require.Equal(t, `{"exit_reason":"error","error":"failed"}`, string(latest))
	archives, err = filepath.Glob(filepath.Join(stdinDir, "task-a.*.out"))
	require.NoError(t, err)
	require.Len(t, archives, 1)
	archived, err := os.ReadFile(archives[0])
	require.NoError(t, err)
	require.Equal(t, firstOutput, archived)
	errArchives, err = filepath.Glob(filepath.Join(stdinDir, "task-a.*.err"))
	require.NoError(t, err)
	require.Len(t, errArchives, 2)
	archivedErr, err := os.ReadFile(errArchives[0])
	require.NoError(t, err)
	require.Equal(t, "stderr tail", string(archivedErr))
	emptyErrArchive, err := os.ReadFile(errArchives[1])
	require.NoError(t, err)
	require.Empty(t, emptyErrArchive)
	latestErr, err = os.ReadFile(filepath.Join(stdinDir, "task-a.err"))
	require.NoError(t, err)
	require.Equal(t, "stderr tail", string(latestErr))
}
