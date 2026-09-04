package shell

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackgroundShellManager_OwnershipAndCloseIsolation(t *testing.T) {
	managerA := NewBackgroundShellManager()
	managerB := NewBackgroundShellManager()
	workingDir := t.TempDir()

	jobA, err := managerA.StartOwned(t.Context(), "session-a", workingDir, nil, "sleep 30", "owner A")
	require.NoError(t, err)
	jobB, err := managerB.StartOwned(t.Context(), "session-b", workingDir, nil, "sleep 30", "owner B")
	require.NoError(t, err)

	_, ok := managerA.GetOwned("session-b", jobA.ID)
	require.False(t, ok)
	require.Error(t, managerA.KillOwned(t.Context(), "session-b", jobA.ID))
	require.False(t, jobA.IsDone(), "a foreign session must not be able to kill the job")

	managerA.Close(t.Context())
	require.True(t, jobA.IsDone(), "closing App A must stop App A jobs")
	require.False(t, jobB.IsDone(), "closing App A must not stop App B jobs")

	managerB.Close(t.Context())
	require.True(t, jobB.IsDone())
}

func TestBackgroundShellManager_CloseRacingStartDoesNotLoseJobs(t *testing.T) {
	manager := NewBackgroundShellManager()
	workingDir := t.TempDir()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var jobs []*BackgroundShell

	for range 16 {
		wg.Go(func() {
			<-start
			job, err := manager.StartOwned(context.Background(), "session", workingDir, nil, "sleep 30", "race")
			if err == nil {
				mu.Lock()
				jobs = append(jobs, job)
				mu.Unlock()
			}
		})
	}

	close(start)
	manager.Close(context.Background())
	wg.Wait()

	require.Empty(t, manager.List())
	require.Zero(t, manager.ActiveJobs())
	mu.Lock()
	defer mu.Unlock()
	for _, job := range jobs {
		require.True(t, job.WaitContext(context.Background()), "every accepted job must be joined by Close")
	}

	_, err := manager.StartOwned(context.Background(), "session", workingDir, nil, "echo should-not-start", "closed")
	require.Error(t, err)
}
