// Retained driver + wake coverage (package R12-D chunk W-B findings 4 and 6):
// the job-INDEPENDENT driver registration surface on HostStore and the wake
// schedule lifecycle, exercised against a database without async_jobs (H4
// final world; newHostTestDB's conditional drop keeps the hardening
// meaningful on any base that still creates the table). Any accidental
// async_jobs query in these paths errors ("no such table: async_jobs"), which
// is what makes these tests real.
package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestHostStore_ClaimSessionDriverInsertsMarker proves claim is idempotent
// and durable on the async_jobs-less DB: two claims succeed, and the
// session_drivers row names THIS store's host.
func TestHostStore_ClaimSessionDriverInsertsMarker(t *testing.T) {
	t.Parallel()
	store, _, q, _, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, q, "s1"))

	require.NoError(t, store.ClaimSessionDriver(ctx, "s1"))
	require.NoError(t, store.ClaimSessionDriver(ctx, "s1"), "a second claim by the same host must be idempotent")

	row, err := q.GetSessionDriver(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "s1", row.SessionID)
	require.Equal(t, id, row.HostID)
}

// TestHostStore_ClaimSessionDriverRefusesForeignLiveHost proves the
// foreign-live refusal: a second HostStore (same conn, same process -- both
// hosts are own-host ids, so hostLivenessDetailFor returns Alive without
// probing) holds the "s2" marker; the first store's claim is refused with
// *ErrSessionDrivenElsewhere naming the second store's host.
func TestHostStore_ClaimSessionDriverRefusesForeignLiveHost(t *testing.T) {
	t.Parallel()
	store, _, q, _, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)

	store2 := NewHostStore(store.sqlDB, t.TempDir(), 998, "test-second")
	t.Cleanup(func() { _ = store2.Close(context.Background()) })
	id2, err := store2.EnsureHost(ctx)
	require.NoError(t, err)
	require.NotEqual(t, id, id2)

	require.NoError(t, seedSession(ctx, q, "s2"))
	require.NoError(t, store2.ClaimSessionDriver(ctx, "s2"))

	err = store.ClaimSessionDriver(ctx, "s2")
	require.Error(t, err)
	var elsewhere *ErrSessionDrivenElsewhere
	require.ErrorAs(t, err, &elsewhere)
	require.Equal(t, "s2", elsewhere.SessionID)
	require.Equal(t, id2, elsewhere.HostID, "the refusal must name the OTHER store's host")
	require.Equal(t, HostStatusAlive, elsewhere.Status)

	// The marker was not stolen.
	row, err := q.GetSessionDriver(ctx, "s2")
	require.NoError(t, err)
	require.Equal(t, id2, row.HostID)
}

// TestHostStore_ClaimSessionDriverTakesOverDeadHost proves dead-host
// takeover: a fabricated marker naming a host whose lock file does not exist
// (ProbeHostLockShared would report Dead) is taken over by claim.
func TestHostStore_ClaimSessionDriverTakesOverDeadHost(t *testing.T) {
	t.Parallel()
	store, conn, q, _, ctx := newHostTestStore(t)

	id, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, q, "s3"))

	// session_drivers.host_id has NO FK to async_hosts (migration
	// 20260929000004), but the dead host's async_hosts row is inserted anyway
	// to mirror a real crashed host (plain INSERT works: async_hosts has no
	// FK either).
	_, err = conn.ExecContext(ctx,
		`INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('dead-host-1', 1, 'dead', 1)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO session_drivers (session_id, host_id, pid, claimed_at) VALUES ('s3', 'dead-host-1', 1, 1)`)
	require.NoError(t, err)

	// Kernel-attested precondition: no lock file -> the host is Dead.
	status, err := ProbeHostLockShared(HostLockPath(store.dataDir, "dead-host-1"))
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, status)

	require.NoError(t, store.ClaimSessionDriver(ctx, "s3"), "a dead host's marker must be taken over")

	row, err := q.GetSessionDriver(ctx, "s3")
	require.NoError(t, err)
	require.Equal(t, id, row.HostID, "the claim must have taken the dead host's marker over")
}

// TestHostStore_ReleaseSessionDriverDeletesOwnRow proves release deletes
// only this store's own marker row.
func TestHostStore_ReleaseSessionDriverDeletesOwnRow(t *testing.T) {
	t.Parallel()
	store, _, q, _, ctx := newHostTestStore(t)

	require.NoError(t, seedSession(ctx, q, "s4"))
	require.NoError(t, store.ClaimSessionDriver(ctx, "s4"))
	_, err := q.GetSessionDriver(ctx, "s4")
	require.NoError(t, err)

	require.NoError(t, store.ReleaseSessionDriver(ctx, "s4"))
	_, err = q.GetSessionDriver(ctx, "s4")
	require.ErrorIs(t, err, sql.ErrNoRows, "release must delete this store's marker row")
}

// TestWakeScheduleOnJobsLessDB proves the full wake lifecycle works on the
// async_jobs-less DB: create (once, contract-valid), claim via ClaimDue's
// lease CAS, fire past RunAt, and find the durable wake_fired session_notices
// row. NOTHING in this path reads async_jobs (the table does not exist).
func TestWakeScheduleOnJobsLessDB(t *testing.T) {
	t.Parallel()
	conn, q, _, ctx := newHostTestDB(t)

	require.NoError(t, seedSession(ctx, q, "wake-owner"))
	now := time.Now().UTC()

	store := NewWakeScheduleStore(conn)
	sched, err := store.CreateSchedule(ctx, CreateWakeScheduleParams{
		Owner:   "wake-owner",
		Kind:    WakeKindOnce,
		Message: "wake up",
		RunAt:   now.Add(time.Minute), // >= MinWakeOnceDelay in the future
	}, now)
	require.NoError(t, err)
	require.Equal(t, "active", sched.State)

	// Claim the due occurrence (lease won by "test-scheduler"): the claim
	// time must be at/after next_run_at (now+1m) for the due scan to see it.
	claimAt := now.Add(time.Minute)
	claimed, err := store.ClaimDue(ctx, "test-scheduler", claimAt, 1, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, sched.ID, claimed[0].ID)

	// Fire with the lease we won, at a now past RunAt.
	fire, err := store.FireOccurrence(ctx, sched.ID, "test-scheduler", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, fire.Fired, "our lease must win the occurrence")
	require.True(t, fire.Done, "a once schedule must reach done with the fire")

	// The durable notice exists: session_notices row for the owner,
	// kind wake_fired.
	var kind, owner string
	err = conn.QueryRowContext(ctx,
		`SELECT kind, owner FROM session_notices WHERE owner = ? AND kind = ?`,
		"wake-owner", NoticeKindWakeFired).Scan(&kind, &owner)
	require.NoError(t, err)
	require.Equal(t, NoticeKindWakeFired, kind)
	require.Equal(t, "wake-owner", owner)

	// The schedule is terminal now.
	row, err := q.GetWakeSchedule(ctx, sched.ID)
	require.NoError(t, err)
	require.Equal(t, "done", row.State)
}
