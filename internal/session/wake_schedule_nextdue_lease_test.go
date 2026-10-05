// NextDue vs live leases (A9-12): NextDueWakeScheduleAt must aim the
// scheduler's timer at the earliest CLAIMABLE moment -- a claimed row's
// lease expiry, not its (past) next_run_at. Without that, a row claimed
// but not fired (fire failed, or the claiming process died) stays "due"
// and every scheduler of the workspace spins: nextWait 0, ClaimDue finds
// nothing claimable, repeat until the lease expires.
//
// REVERT CHECK: reverting NextDueWakeScheduleAt to MIN(w.next_run_at)
// makes the phase-1 assertion below fail: NextDue reports the past
// next_run_at of the leased row (the busy-spin window) instead of its
// lease expiry.

package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A9-12, three phases: a live lease defers the row's due moment to the
// lease expiry; an unclaimed row still reports its own (earlier)
// next_run_at; once the lease expires (recovery sweep), the row is back
// to plain next_run_at semantics.
func TestWakeScheduleStore_NextDueAimsAtClaimableMoment(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	due := f.base.Add(time.Minute)

	// Phase 1: the only schedule is due and leased by a worker that has
	// not fired yet (5-minute TTL). NextDue must report the lease expiry.
	rowA := f.create(WakeKindOnce, due, nil)
	claimed, err := f.wake.ClaimDue(f.ctx, "worker-a", due, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, rowA.ID, claimed[0].ID)
	expiry := time.Unix(claimed[0].LeaseExpiresAt.Int64, 0).UTC()
	next, err := f.wake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.True(t, next.Equal(expiry),
		"NextDue must aim at the live lease expiry %v, got %v", expiry, next)

	// Phase 2: an unclaimed schedule with an earlier next_run_at must
	// still dominate the timer (the fix must not delay free rows).
	early := due.Add(-30 * time.Second)
	rowB := f.create(WakeKindOnce, early, nil)
	next, err = f.wake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.True(t, next.Equal(early),
		"NextDue must stay the unclaimed row's earlier next_run_at, got %v", next)

	// Phase 3: B cancelled and A's lease expired (the recovery sweep
	// clears it): NextDue is back to next_run_at semantics -- A's (past)
	// next_run_at, i.e. the row is due and claimable again.
	require.NoError(t, f.wake.CancelSchedule(f.ctx, rerunOwner, rowB.ID, f.base))
	_, err = f.wake.RecoverExpiredLeases(f.ctx, expiry.Add(time.Second))
	require.NoError(t, err)
	next, err = f.wake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.True(t, next.Equal(due),
		"after the lease expiry NextDue must be the row's next_run_at again, got %v", next)
}
