package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestShutdownWithResult_ForcedSkipsDBReleaseAndReportsForced pins the
// forced-shutdown contract of ShutdownWithResult: when CancelAll reports
// still busy, the DB is NOT released (live writers are protected) and the
// result is flagged Forced with no cleanup errors.
func TestShutdownWithResult_ForcedSkipsDBReleaseAndReportsForced(t *testing.T) {
	dataDir := t.TempDir()
	entry, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.NotNil(t, entry)

	mockCoord := &mockCoordinatorForShutdown{
		cancelAllFunc: func() (stillBusy bool) {
			return true
		},
	}

	appInstance := &App{
		AgentCoordinator: mockCoord,
		DB:               func() *sql.DB { return nil },
		dataDir:          dataDir,
		dbReleasesNeeded: 1,
		globalCtx:        context.Background(),
	}

	res := appInstance.ShutdownWithResult()
	require.True(t, res.Forced)
	require.Empty(t, res.CleanupErrors)

	// Forced shutdown must have skipped db.Release: a second Connect
	// returns the SAME pool entry (refcount never hit zero).
	entry2, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.Same(t, entry, entry2, "same pool entry means DB was not released")

	// Teardown: refcount is 2 and forced shutdown skipped Release, so
	// ReleaseAll forcibly removes and closes the pooled entry.
	require.NoError(t, db.ReleaseAll(dataDir))
}

// TestShutdownWithResult_GracefulReleasesDBAndReportsNotForced pins the
// graceful-shutdown contract of ShutdownWithResult: when nothing is busy,
// the DB IS released (pool entry dropped at refcount zero) and the result
// is not flagged Forced and carries no cleanup errors.
func TestShutdownWithResult_GracefulReleasesDBAndReportsNotForced(t *testing.T) {
	dataDir := t.TempDir()
	entry, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.NotNil(t, entry)

	mockCoord := &mockCoordinatorForShutdown{
		cancelAllFunc: func() (stillBusy bool) {
			return false
		},
	}

	appInstance := &App{
		AgentCoordinator: mockCoord,
		DB:               func() *sql.DB { return nil },
		dataDir:          dataDir,
		dbReleasesNeeded: 1,
		globalCtx:        context.Background(),
	}

	res := appInstance.ShutdownWithResult()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)

	// Graceful shutdown must have released the DB: the original pool
	// entry hit refcount zero and was removed, so this Connect creates
	// a fresh entry.
	entry2, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	require.NotSame(t, entry, entry2, "different entry means old one was released")

	// Teardown: release the fresh entry created above.
	require.NoError(t, db.Release(dataDir))
}

// TestShutdownWithResult_CollectsCleanupErrors pins the error-collection
// contract of ShutdownWithResult: errors returned by cleanup functions are
// gathered into the returned snapshot instead of being discarded.
func TestShutdownWithResult_CollectsCleanupErrors(t *testing.T) {
	sentinel := errors.New("cleanup boom")

	appInstance := &App{
		cleanupFuncs: []func(context.Context) error{
			func(context.Context) error {
				return sentinel
			},
		},
	}

	res := appInstance.ShutdownWithResult()
	require.Len(t, res.CleanupErrors, 1)
	require.ErrorIs(t, res.CleanupErrors[0], sentinel)
}
