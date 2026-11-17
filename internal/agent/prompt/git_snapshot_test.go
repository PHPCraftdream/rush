package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The git status block precedes the skills, the project context and the whole
// conversation, so a prompt that renders a different status at every turn start
// moves the provider's cache prefix every time. Two Builds in one process must
// render the same block until the snapshot expires.
func TestBuild_GitStatusSnapshotIsStableAcrossTurns(t *testing.T) {
	store := testConfigStore(t)
	require.NoError(t, os.Mkdir(filepath.Join(store.WorkingDir(), ".git"), 0o755))

	p := newTestCoderPrompt(t, store.WorkingDir())
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	reads := 0
	status := "Status: clean\n"
	p.gitStatusFn = func(context.Context, string) (string, error) {
		reads++
		return status, nil
	}

	first, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)
	require.Contains(t, first, "Status: clean")

	// The worktree changes between turns; the next turn start must not see it.
	status = "Status:\n M internal/agent/agent.go\n"
	now = now.Add(30 * time.Minute)
	second, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)
	require.Equal(t, first, second, "the system prompt moved between turns, so the provider cache prefix breaks at the status block")
	require.Equal(t, 1, reads, "git status was re-read inside the snapshot window")

	// After the TTL the snapshot is refreshed once.
	now = now.Add(gitSnapshotTTL)
	third, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)
	require.True(t, strings.Contains(third, "internal/agent/agent.go"), "an expired snapshot must be re-read")
	require.Equal(t, 2, reads)
}

// A failed read is surfaced and not cached, so the next Build retries.
func TestGitStatusSnapshot_FailureIsNotCached(t *testing.T) {
	p := newTestCoderPrompt(t, t.TempDir())
	calls := 0
	p.gitStatusFn = func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			return "", os.ErrPermission
		}
		return "Status: clean\n", nil
	}
	_, err := p.gitStatusSnapshot(context.Background(), "dir")
	require.Error(t, err)
	got, err := p.gitStatusSnapshot(context.Background(), "dir")
	require.NoError(t, err)
	require.Equal(t, "Status: clean\n", got)
	require.Equal(t, 2, calls)
}
