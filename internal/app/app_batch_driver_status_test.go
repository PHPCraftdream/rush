package app

import (
	"context"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestBatchDriverStatus_OwnHostAlive pins the own-host shortcut in
// batchCutoverHostLiveness: a host id registered as this process's own
// answers Alive without touching the shared probe.
//
// Revert-check: removing the session.IsOwnHostID shortcut makes the probe
// return Unknown for own hosts, failing the Status==Alive assertion first.
func TestBatchDriverStatus_OwnHostAlive(t *testing.T) {
	app := newFullNewApp(t)
	ctx := context.Background()
	sess, err := app.Sessions.Create(ctx, "ph2-own-alive")
	require.NoError(t, err)
	require.NoError(t, app.BatchHostStore().ClaimSessionDriver(ctx, sess.ID))
	require.NotEmpty(t, app.BatchHostStore().HostID())

	got, err := app.LiveSessionDrivers(ctx)

	require.NoError(t, err)
	require.Contains(t, got, sess.ID)
	require.Equal(t, session.HostStatusAlive, got[sess.ID].Status)
}

// TestBatchDriverStatus_ForeignLockedHostAlive pins the conservative probe
// verdict for a foreign host whose lock file is held: the marker stays and
// carries the row's PID.
//
// Revert-check: collapsing foreign-alive to Unknown or Dead fails the
// Status assertion.
func TestBatchDriverStatus_ForeignLockedHostAlive(t *testing.T) {
	app := newFullNewApp(t)
	ctx := context.Background()
	dataDir := app.dataDir
	foreignHost := "ph2-foreign-live-host"
	sess, err := app.Sessions.Create(ctx, "ph2-foreign-alive")
	require.NoError(t, err)
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, foreignHost))
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })
	_, err = app.DB().ExecContext(ctx,
		`INSERT INTO session_drivers (session_id, host_id, pid, claimed_at) VALUES (?, ?, ?, ?)`,
		sess.ID, foreignHost, 5151, 1)
	require.NoError(t, err)

	got, err := app.LiveSessionDrivers(ctx)

	require.NoError(t, err)
	require.Contains(t, got, sess.ID)
	require.Equal(t, session.HostStatusAlive, got[sess.ID].Status)
	require.Equal(t, int64(5151), got[sess.ID].PID)
}

// TestBatchDriverStatus_ForeignDeadHostDropped pins the Dead filter: a
// foreign host whose lock file remains but is unlocked is provably dead and
// its marker is dropped.
//
// Revert-check: stopping the Dead filter keeps the dead host's marker,
// failing the NotContains assertion.
func TestBatchDriverStatus_ForeignDeadHostDropped(t *testing.T) {
	app := newFullNewApp(t)
	ctx := context.Background()
	dataDir := app.dataDir
	foreignHost := "ph2-foreign-dead-host"
	sess, err := app.Sessions.Create(ctx, "ph2-foreign-dead")
	require.NoError(t, err)
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, foreignHost))
	require.NoError(t, err)
	require.NoError(t, lk.Release())
	_, err = app.DB().ExecContext(ctx,
		`INSERT INTO session_drivers (session_id, host_id, pid, claimed_at) VALUES (?, ?, ?, ?)`,
		sess.ID, foreignHost, 5151, 1)
	require.NoError(t, err)

	got, err := app.LiveSessionDrivers(ctx)

	require.NoError(t, err)
	require.NotContains(t, got, sess.ID)
}

// TestBatchDriverStatus_ProbeErrorStaysUnknown pins the error mapping: when
// the probe cannot decide (a directory sits at the lock path) the host is
// Unknown and its marker is kept -- only Dead is dropped.
//
// Revert-check: mapping probe errors to Dead drops the marker, failing the
// Contains assertion.
func TestBatchDriverStatus_ProbeErrorStaysUnknown(t *testing.T) {
	app := newFullNewApp(t)
	ctx := context.Background()
	dataDir := app.dataDir
	foreignHost := "ph2-foreign-error-host"
	sess, err := app.Sessions.Create(ctx, "ph2-foreign-error")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(session.HostLockPath(dataDir, foreignHost), 0o755))
	_, err = app.DB().ExecContext(ctx,
		`INSERT INTO session_drivers (session_id, host_id, pid, claimed_at) VALUES (?, ?, ?, ?)`,
		sess.ID, foreignHost, 5151, 1)
	require.NoError(t, err)

	got, err := app.LiveSessionDrivers(ctx)

	require.NoError(t, err)
	require.Contains(t, got, sess.ID)
	require.Equal(t, session.HostStatusUnknown, got[sess.ID].Status)
}
