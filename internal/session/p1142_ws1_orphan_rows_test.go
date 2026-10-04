// WS-1 orphan rows (task #1142 SD-C): an owner session that no longer has a
// sessions row must not make its durable wake/run-queue rows invisible.
//
// Step 1 of #1142 implemented the WS-1 ownership filter as an INNER JOIN
// sessions ON s.id = w.owner_session_id, which silently dropped every row
// whose session was gone (history wiped, or written while foreign keys were
// off) -- including the fixtures the agent wake tests use, whose schedules
// point at session ids that were never inserted. The result was a NULL from
// NextDueWakeScheduleAt, a scheduler timer parked on the idle cap, and a
// claim that could never deliver anything (an empty claim in a loop). The
// filter now LEFT JOINs and COALESCEs the missing session to the legacy
// unbound value, so such a row is still VISIBLE to the one process that may
// execute it -- the home process -- and still invisible to every
// linked-worktree process, exactly like a legacy unbound row.
//
// Operationally an orphaned schedule can never be executed by anyone but the
// home process (firing it starts a turn of a session that no longer exists),
// which is why the fix is "home sees it" and not "everyone sees it": WS-1's
// fail-closed property is preserved for linked processes.

package session

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// ghostOwner is an owner session id with NO sessions row, before and after
// the orphan rows are written below.
const ghostOwner = "ghost-owner-no-session-row"

// relaxForeignKeys lets the fixture write rows that reference a session that
// does not exist -- the pre-WS-1 shape of the world this test reproduces (the
// agent package's own wake fixture relies on the same trick). The writer is a
// single pooled connection, so the pragma applies to every subsequent write,
// and it is restored at the end of the test: the orphan rows already on disk
// stay orphaned, SQLite never re-validates existing rows.
func (f *wsFx) relaxForeignKeys() {
	f.t.Helper()
	_, err := f.conn.ExecContext(f.ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(f.t, err)
	f.t.Cleanup(func() {
		_, err := f.conn.ExecContext(f.ctx, `PRAGMA foreign_keys = ON`)
		require.NoError(f.t, err)
	})
}

// orphanWakeSchedule writes an active schedule for an owner that has no
// sessions row.
func (f *wsFx) orphanWakeSchedule(owner string, runAt, base time.Time) db.WakeSchedule {
	f.t.Helper()
	row, err := NewWakeScheduleStore(f.conn).CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: owner, Kind: WakeKindOnce, Message: "orphan", RunAt: runAt,
	}, base)
	require.NoError(f.t, err)
	return row
}

// orphanRunQueueEntry writes a pending run-queue row for an owner that has no
// sessions row. It is inserted directly rather than through
// EnqueueRunQueueEntry so the row keeps an explicit, deterministic created_at.
func (f *wsFx) orphanRunQueueEntry(id, sessionID string, createdAt int64) {
	f.t.Helper()
	_, err := f.conn.ExecContext(f.ctx, `
INSERT INTO session_run_queue (id, session_id, call_data, status, attempts, terminal_failure, created_at, updated_at)
VALUES (?, ?, '{}', 'pending', 0, 0, ?, ?)`, id, sessionID, createdAt, createdAt)
	require.NoError(f.t, err)
}

// TestWS1_OrphanRows_HomeSeesLinkedDoesNot is the SD-C regression test for
// both ownership-filtered scans at once.
//
// Revert-check (mutant: the LEFT JOIN + COALESCE predicate reverted to the
// INNER JOIN + bare s.workspace_root predicate of #1142 step 1):
//  1. In wake_schedules.sql, restore `JOIN sessions s ON s.id = w.owner_session_id`
//     with `s.workspace_root = sqlc.arg(workspace_root)` in ListDueWakeSchedules
//     and NextDueWakeScheduleAt, and the same in run_queue.sql's
//     ListPendingRunQueueEntries, then regenerate.
//  2. Run: go test ./internal/session/ -run TestWS1_OrphanRows_HomeSeesLinkedDoesNot -v
//  3. FAIL at the home-side assertions: every orphan row is dropped by the
//     join, so the home process sees nothing at all -- NextDue would be nil
//     (the scheduler parks on its idle cap), ClaimDue returns zero rows, and
//     the home scan lists zero pending entries.
//  4. Restore the LEFT JOIN + COALESCE and PASS.
func TestWS1_OrphanRows_HomeSeesLinkedDoesNot(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := newWsFx(t)
	f.relaxForeignKeys()

	// The owners that DO have a sessions row, one of them foreign, so every
	// branch of the predicate is exercised: own, foreign, orphaned.
	f.seedSession("wake-own", wsOwn, 0)
	f.seedSession("wake-foreign", wsForeign, 0)
	f.seedSession("queue-own", wsOwn, 0)
	f.seedSession("queue-foreign", wsForeign, 0)

	// The orphaned row is the EARLIEST of the three schedules: a filter that
	// drops it reports a later due moment to the home process, which is the
	// observable shape of the bug (the scheduler parks on the idle cap).
	orphanDue := base.Add(time.Hour)
	foreignDue := base.Add(30 * time.Minute)
	ownDue := base.Add(2 * time.Hour)

	orphanWake := f.orphanWakeSchedule(ghostOwner, orphanDue, base)
	foreignWake := f.orphanWakeSchedule("wake-foreign", foreignDue, base)
	ownWake := f.orphanWakeSchedule("wake-own", ownDue, base)

	// Run-queue rows: one owned, one foreign, one orphaned.
	f.orphanRunQueueEntry("rq-orphan", ghostOwner, base.Unix())
	f.orphanRunQueueEntry("rq-own", "queue-own", base.Unix())
	f.orphanRunQueueEntry("rq-foreign", "queue-foreign", base.Unix())

	homeWake := NewWakeScheduleStore(f.conn)                                  // workspace "" = home
	linkedWake := NewWakeScheduleStoreWithWorkspace(f.conn, wsOwn, "", false) // shared-from-linked
	require.NotEmpty(t, orphanWake.ID)
	require.NotEmpty(t, foreignWake.ID)
	require.NotEmpty(t, ownWake.ID)

	// --- wake: NextDue -------------------------------------------------
	// The home process sees the orphaned row and reports its (earliest) due
	// moment. With the INNER JOIN this was NULL: the row had no left side.
	nextHome, err := homeWake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, nextHome, "a home process must still see an orphaned owner's schedule")
	require.True(t, nextHome.Equal(orphanDue),
		"home NextDue must be the orphaned row's due time, got %v", nextHome)

	// A linked worktree process sees only its own row: neither the foreign
	// schedule nor the orphaned one (which it has no session to fire).
	nextLinked, err := linkedWake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, nextLinked)
	require.True(t, nextLinked.Equal(ownDue),
		"a linked process's NextDue must be its own due time, got %v", nextLinked)

	// --- wake: ClaimDue ------------------------------------------------
	claimAt := base.Add(3 * time.Hour)
	claimedHome, err := homeWake.ClaimDue(f.ctx, "pump-home", claimAt, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimedHome, 1, "the home process must be able to claim the orphaned row")
	require.Equal(t, orphanWake.ID, claimedHome[0].ID)
	require.Equal(t, "pump-home", claimedHome[0].LeaseOwner.String)

	claimedLinked, err := linkedWake.ClaimDue(f.ctx, "pump-linked", claimAt, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimedLinked, 1, "the linked process claims exactly its own row")
	require.Equal(t, ownWake.ID, claimedLinked[0].ID)

	// The orphaned row stays leased BY the home process only: the linked
	// claim never touched it.
	storedOrphan, err := f.q.GetWakeSchedule(f.ctx, orphanWake.ID)
	require.NoError(t, err)
	require.Equal(t, "pump-home", storedOrphan.LeaseOwner.String)
	require.Equal(t, "active", storedOrphan.State)

	// --- run queue: the pump scan --------------------------------------
	svcHome := f.service("", "")
	svcLinked := f.serviceLinked(wsOwn, "")

	home, err := svcHome.ListPendingRunQueueEntries(f.ctx)
	require.NoError(t, err)
	require.Len(t, home, 1, "a home process scans the orphaned row, not its own bound sessions")
	require.Equal(t, "rq-orphan", home[0].ID)
	require.Equal(t, ghostOwner, home[0].SessionID)

	linked, err := svcLinked.ListPendingRunQueueEntries(f.ctx)
	require.NoError(t, err)
	require.Len(t, linked, 1, "a linked process scans exactly its own session's row")
	require.Equal(t, "rq-own", linked[0].ID)

	// --- run queue: the claim ------------------------------------------
	// The claim itself is keyed by session id and carries no ownership
	// predicate on purpose (the pump only ever reaches it for a row its own
	// scan handed it), so the ownership gate really is the scan above. Here
	// the home process resolves the orphaned row the way its pump would:
	// leased, never deleted, attempts untouched.
	leased, err := svcHome.LeaseRunQueueEntry(f.ctx, ghostOwner, "pump-home", 30*time.Second)
	require.NoError(t, err)
	require.NotNil(t, leased, "the home process must be able to lease the orphaned row")
	require.Equal(t, "rq-orphan", leased.ID)
	require.EqualValues(t, 0, leased.Attempts)

	after, err := svcHome.ListPendingRunQueueEntries(f.ctx)
	require.NoError(t, err)
	require.Empty(t, after, "once the home process leases it, the orphaned row is no longer pending")

	// The foreign row was never visible to either scan and is untouched.
	foreignRow, err := f.q.GetRunQueueEntry(f.ctx, "rq-foreign")
	require.NoError(t, err)
	require.Equal(t, "pending", foreignRow.Status)
	require.Equal(t, int64(0), foreignRow.Attempts)
}
