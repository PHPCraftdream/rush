// Scheduler-level guard for A9-12: while a due row is held by a live
// lease of ANOTHER owner, the scheduler's computed wait must aim at the
// lease, never at the row's (past) next_run_at. A zero or negative wait
// here is the busy spin: fireDue opens a claim transaction, finds nothing
// claimable, the loop recomputes the same zero, repeat -- until the lease
// expires. The wait itself stays capped by wakeSchedulerMaxWait (the
// cross-process recompute cadence), so the observable contract is:
// remainder < cap -> wait == remainder; remainder > cap -> wait == cap;
// lease expired -> wait == 0 (claimable again).
//
// REVERT CHECK: reverting NextDueWakeScheduleAt to MIN(w.next_run_at)
// makes the first two wait assertions below fail: the waits collapse to
// 0 while the foreign lease is live.

package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestWakeScheduler_LiveForeignLeaseSetsNonzeroWait(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	// A once schedule already due, leased by ANOTHER owner with a short
	// TTL (40s remainder < the 1-minute recompute cap): the timer must
	// sleep exactly the lease remainder, not spin on next_run_at.
	_, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 5*time.Second), fx.now())
	require.NoError(t, err)
	fx.clock.Store(fx.now().Add(10 * time.Second).UnixNano())
	shortLease := 40 * time.Second
	claimed, err := fx.store.ClaimDue(fx.ctx, "other-owner", fx.now(), 10, shortLease)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	remainder := time.Duration(claimed[0].LeaseExpiresAt.Int64-fx.now().Unix()) * time.Second
	require.Equal(t, shortLease, remainder)
	wait := fx.reschedule(s)
	require.Equal(t, remainder, wait,
		"the wait must be the live lease's remainder, got %v", wait)

	// With the default 5-minute TTL the remainder exceeds the recompute
	// cap: the wait is the cap, still never zero/negative (that zero is
	// the busy-spin shape the MIN(w.next_run_at) query produced).
	fx.clock.Store(fx.now().Add(50 * time.Second).UnixNano()) // short lease now expired
	claimed, err = fx.store.ClaimDue(fx.ctx, "other-owner", fx.now(), 10, session.WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	wait = fx.reschedule(s)
	require.Equal(t, wakeSchedulerMaxWait, wait,
		"a remainder past the cap must yield the cap, never 0, got %v", wait)

	// Well past that lease too the row is claimable again: the wait
	// collapses to 0, now legitimately (the next tick claims and fires).
	fx.clock.Store(fx.now().Add(2 * session.WakeLeaseTTL).UnixNano())
	wait = fx.reschedule(s)
	require.Equal(t, time.Duration(0), wait,
		"past the lease expiry the row is due and claimable again")
}
