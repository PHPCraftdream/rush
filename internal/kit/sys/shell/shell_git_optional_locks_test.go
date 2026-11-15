package shell

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewShell_SetsGitOptionalLocks pins A8: shells the agent launches
// carry GIT_OPTIONAL_LOCKS=0 so a timed-out read-only git command cannot
// leave a stale .git/worktrees/<n>/index.lock behind.
func TestNewShell_SetsGitOptionalLocks(t *testing.T) {
	t.Parallel()

	s := NewShell(&Options{})
	require.True(t, hasEnvVar(s.GetEnv(), "GIT_OPTIONAL_LOCKS"))
	require.Contains(t, s.GetEnv(), "GIT_OPTIONAL_LOCKS=0")
}

// An explicit user value must not be clobbered.
func TestNewShell_PreservesExplicitGitOptionalLocks(t *testing.T) {
	t.Parallel()

	env := []string{"PATH=/bin", "GIT_OPTIONAL_LOCKS=1"}
	s := NewShell(&Options{Env: env})
	for _, entry := range s.GetEnv() {
		require.NotEqual(t, "GIT_OPTIONAL_LOCKS=0", entry)
	}
	found := false
	for _, entry := range s.GetEnv() {
		if strings.HasPrefix(entry, "GIT_OPTIONAL_LOCKS=") {
			found = true
			require.Equal(t, "GIT_OPTIONAL_LOCKS=1", entry)
		}
	}
	require.True(t, found)
}
