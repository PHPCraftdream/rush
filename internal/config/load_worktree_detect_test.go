// Regression tests for git-worktree detection in Load: the no-repo
// file-walk limits (ls/tui-completion depth=2, items=100) must key on
// the working directory passed to Load, never on the process's own
// CWD. An embeddable SDK host (sdk.Open + Options.WorkingDir) routinely
// runs with its process CWD somewhere else entirely, so a CWD-based
// probe answers the wrong question — and a process-wide sync.Once
// memoization then freezes that wrong answer for the process lifetime.
//
// The two subtests are complementary by construction: on the buggy
// code the once-cached CWD probe returns a single process-wide answer,
// so exactly one of the two directions must fail no matter which way
// the cache got poisoned (in-repo or non-repo) by earlier tests.
package config

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireWalkLimits asserts whether Load applied the no-git-repo
// file-walk limits (depth=2, items=100) to the ls tool and TUI
// completion config. Applied means non-nil pointers with the limit
// values; not applied means the pointers stay nil (assignIfNil).
func requireWalkLimits(t *testing.T, store *ConfigStore, wantLimits bool) {
	t.Helper()

	cfg := store.Config()
	if wantLimits {
		require.NotNil(t, cfg.Tools.Ls.MaxDepth, "ls max_depth limit must be applied")
		require.NotNil(t, cfg.Tools.Ls.MaxItems, "ls max_items limit must be applied")
		require.Equal(t, 2, *cfg.Tools.Ls.MaxDepth)
		require.Equal(t, 100, *cfg.Tools.Ls.MaxItems)
		require.NotNil(t, cfg.Options.TUI.Completions.MaxDepth)
		require.NotNil(t, cfg.Options.TUI.Completions.MaxItems)
		require.Equal(t, 2, *cfg.Options.TUI.Completions.MaxDepth)
		require.Equal(t, 100, *cfg.Options.TUI.Completions.MaxItems)
		return
	}
	require.Nil(t, cfg.Tools.Ls.MaxDepth, "working dir is inside a git repo: ls max_depth must stay unlimited")
	require.Nil(t, cfg.Tools.Ls.MaxItems, "working dir is inside a git repo: ls max_items must stay unlimited")
}

func TestLoad_WalkLimitsKeyedOnWorkingDirNotProcessCWD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateAllGlobalConfigPaths(t)

	// A real git repository to pass as Load's working directory,
	// isolated from the test process's own location (mirrors the
	// git-init pattern used by load_discovery_test.go).
	gitRepo := t.TempDir()
	gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
	gitInit.Dir = gitRepo
	require.NoError(t, gitInit.Run())

	// A plain directory outside any repository: serves as the
	// out-of-repo working dir and, via t.Chdir, as an out-of-repo
	// process CWD.
	plainDir := t.TempDir()

	t.Run("working dir inside a git repo gets no walk limits even when process CWD is outside any repo", func(t *testing.T) {
		// Simulate an SDK host: process CWD detached from workingDir.
		t.Chdir(plainDir)

		store, err := Load(gitRepo, t.TempDir(), false)
		require.NoError(t, err)
		requireWalkLimits(t, store, false)
	})

	t.Run("working dir outside any git repo gets walk limits even when process CWD is inside one", func(t *testing.T) {
		// A repo CWD must not mask that the working dir has no repo.
		t.Chdir(gitRepo)

		store, err := Load(plainDir, t.TempDir(), false)
		require.NoError(t, err)
		requireWalkLimits(t, store, true)
	})
}
