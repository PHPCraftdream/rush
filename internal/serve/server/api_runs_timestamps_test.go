package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAPIRunRegistryRecordsStartAndFinishTimes(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	registry := NewAPIRunRegistry(func() time.Time { return now })
	run, err := registry.Create("session", "token")
	require.NoError(t, err)
	require.Nil(t, run.StartedAt)
	require.Nil(t, run.FinishedAt)

	now = now.Add(time.Second)
	run, err = registry.Transition(run.ID, APIRunRunning, "", "", "")
	require.NoError(t, err)
	require.NotNil(t, run.StartedAt)
	require.Equal(t, now, *run.StartedAt)
	require.Nil(t, run.FinishedAt)

	now = now.Add(time.Second)
	run, err = registry.Transition(run.ID, APIRunSucceeded, "", "", "msg-1")
	require.NoError(t, err)
	require.Equal(t, now.Add(-time.Second), *run.StartedAt, "the start time is kept")
	require.NotNil(t, run.FinishedAt)
	require.Equal(t, now, *run.FinishedAt)
	require.Equal(t, "msg-1", run.MessageID)
}
