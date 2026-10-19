package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// budgetReserveRoot reserves one root-only batch (no children) for owner on
// the given host. Once announced, each root contributes exactly one
// interrupted node and one recovery notice.
func budgetReserveRoot(t *testing.T, s *BatchStore, ctx context.Context, toolCallID, host string) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.Host = host
	p.ToolCallID = toolCallID
	p.Root.Children = nil
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

func budgetDeadHost(t *testing.T, dir, host string) {
	t.Helper()
	lock, err := TryAcquireFileLock(HostLockPath(dir, host))
	require.NoError(t, err)
	require.NoError(t, lock.Release())
}

func TestBatchRecoveryPassPagesAndRecoversAllRoots(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	const roots = 5
	for i := range roots {
		snap := budgetReserveRoot(t, a, ctx, fmt.Sprintf("call-%d", i), "host")
		r9Announce(t, conn, ctx, snap.Root.RootID)
	}
	budgetDeadHost(t, dir, "host")
	var total BatchRecoveryOutcome
	cursor := BatchRecoveryCursorStart
	seenCursors := map[BatchRecoveryCursor]bool{}
	passes := 0
	for {
		out, next, done, err := a.BatchRecoverPass(ctx, BatchSharedHostProbe(dir), BatchRecoveryLimits{MaxRoots: 2, PageSize: 2}, cursor)
		require.NoError(t, err)
		require.LessOrEqual(t, out.Interrupted, int64(2), "a pass must never attempt more roots than MaxRoots")
		require.False(t, seenCursors[next], "cursor must advance; later roots must not be starved")
		if !done {
			seenCursors[next] = true
		}
		total.Interrupted += out.Interrupted
		total.Notices += out.Notices
		cursor = next
		passes++
		if done {
			break
		}
		require.Less(t, passes, 10, "repeated passes did not converge")
	}
	require.Equal(t, 3, passes)
	require.EqualValues(t, roots, total.Interrupted)
	require.EqualValues(t, roots, total.Notices)
	require.Equal(t, BatchRecoveryCursorStart, cursor)
	require.Len(t, seenCursors, 2)
	out, _, done, err := a.BatchRecoverPass(ctx, BatchSharedHostProbe(dir), BatchRecoveryLimits{MaxRoots: 64}, BatchRecoveryCursorStart)
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, BatchRecoveryOutcome{}, out, "a finished pass must find no recoverable roots left")
}

func TestBatchRecoveryPassCancelledBetweenRootsStopsCleanly(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	snaps := make([]BatchSnapshot, 3)
	for i := range snaps {
		snaps[i] = budgetReserveRoot(t, a, ctx, fmt.Sprintf("call-%d", i), "host")
		r9Announce(t, conn, ctx, snaps[i].Root.RootID)
	}
	budgetDeadHost(t, dir, "host")
	probeCtx, cancel := context.WithCancel(context.Background())
	probe := func(string) (HostLockStatus, error) {
		cancel()
		return HostStatusDead, nil
	}
	out, cursor, done, err := a.BatchRecoverPass(probeCtx, probe, BatchRecoveryLimits{MaxRoots: 3}, BatchRecoveryCursorStart)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, done, "a cancelled pass must report it stopped early")
	require.Zero(t, out.Interrupted)
	require.Equal(t, BatchRecoveryCursorStart, cursor, "no root was attempted, so the cursor must not advance")
	for _, snap := range snaps {
		got, err := a.Read(ctx, "owner", snap.Root.RootID)
		require.NoError(t, err)
		require.Equal(t, snap, got, "a cancelled pass must leave no partial root")
	}
	out, cursor, done, err = a.BatchRecoverPass(ctx, BatchSharedHostProbe(dir), BatchRecoveryLimits{MaxRoots: 3}, BatchRecoveryCursorStart)
	require.NoError(t, err)
	require.True(t, done)
	require.EqualValues(t, 3, out.Interrupted)
	require.EqualValues(t, 3, out.Notices)
	require.Equal(t, BatchRecoveryCursorStart, cursor)
}

func TestBatchRecoveryPassVerdictsAndProbeOnce(t *testing.T) {
	a, _, _, dir, ctx := r9Stores(t)
	snaps := make([]BatchSnapshot, 3)
	for i := range snaps {
		snaps[i] = budgetReserveRoot(t, a, ctx, fmt.Sprintf("call-%d", i), "host")
	}
	type verdictCase struct {
		name         string
		probe        BatchHostProbe
		wantProbeErr bool
	}
	cases := []verdictCase{
		{"alive", func(string) (HostLockStatus, error) { return HostStatusAlive, nil }, false},
		{"unknown", func(string) (HostLockStatus, error) { return HostStatusUnknown, errors.New("no verdict") }, true},
		{"dead-with-error", func(string) (HostLockStatus, error) { return HostStatusDead, errors.New("probe boom") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			counting := func(host string) (HostLockStatus, error) {
				probes++
				return tc.probe(host)
			}
			out, cursor, done, err := a.BatchRecoverPass(ctx, counting, BatchRecoveryLimits{MaxRoots: 10}, BatchRecoveryCursorStart)
			if tc.wantProbeErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.True(t, done)
			require.Equal(t, BatchRecoveryCursorStart, cursor)
			require.Equal(t, 1, probes, "each distinct host must be probed at most once per pass")
			require.Equal(t, BatchRecoveryOutcome{}, out, "a non-confirming verdict must write nothing")
			for _, snap := range snaps {
				got, err := a.Read(ctx, "owner", snap.Root.RootID)
				require.NoError(t, err)
				require.Equal(t, snap, got)
			}
		})
	}
	t.Run("own-host", func(t *testing.T) {
		markOwnHostID("host")
		defer unmarkOwnHostID("host")
		probes := 0
		out, _, _, err := a.BatchRecoverPass(ctx, func(host string) (HostLockStatus, error) {
			probes++
			return HostStatusDead, nil
		}, BatchRecoveryLimits{MaxRoots: 10}, BatchRecoveryCursorStart)
		require.NoError(t, err)
		require.Zero(t, probes, "an own host must never be probed")
		require.Equal(t, BatchRecoveryOutcome{}, out)
	})
}

func TestRecoverHostsOnceBudgetAndCompletion(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	hosts := []string{"", "own-host", "h1", "h1", "h2", "h3"}
	ids := make(map[string]string, 3)
	for _, host := range []string{"h1", "h2", "h3"} {
		snap := budgetReserveRoot(t, a, ctx, "call-"+host, host)
		ids[host] = snap.Root.RootID
		r9Announce(t, conn, ctx, snap.Root.RootID)
		budgetDeadHost(t, dir, host)
	}
	markOwnHostID("own-host")
	defer unmarkOwnHostID("own-host")
	out, more, err := a.RecoverHostsOnce(ctx, BatchSharedHostProbe(dir), hosts, 2)
	require.NoError(t, err)
	require.True(t, more, "budget exhaustion must report more remaining")
	require.EqualValues(t, 2, out.Interrupted)
	got, err := a.Read(ctx, "owner", ids["h3"])
	require.NoError(t, err)
	require.Equal(t, "pending", got.Root.State, "an unattempted host's root must stay untouched")
	got, err = a.Read(ctx, "owner", ids["h1"])
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.Root.State)
	out, more, err = a.RecoverHostsOnce(ctx, BatchSharedHostProbe(dir), hosts, 2)
	require.NoError(t, err)
	require.False(t, more)
	require.EqualValues(t, 1, out.Interrupted)
	for _, host := range []string{"h1", "h2", "h3"} {
		got, err := a.Read(ctx, "owner", ids[host])
		require.NoError(t, err)
		require.Equal(t, "interrupted", got.Root.State)
		require.Equal(t, "host_lost", got.Root.OutcomeCode)
		require.Zero(t, got.Root.Wake)
	}
}

func TestBatchRecoverRootExactNamedRoot(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	first := budgetReserveRoot(t, a, ctx, "call-1", "host")
	second := budgetReserveRoot(t, a, ctx, "call-2", "host")
	r9Announce(t, conn, ctx, first.Root.RootID)
	r9Announce(t, conn, ctx, second.Root.RootID)
	budgetDeadHost(t, dir, "host")
	out, err := a.BatchRecoverRoot(ctx, second.Root.RootID, BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.EqualValues(t, 1, out.Interrupted)
	require.EqualValues(t, 1, out.Notices)
	got, err := a.Read(ctx, "owner", first.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, first, got, "an exact-root recovery must not touch other roots")
	got, err = a.Read(ctx, "owner", second.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.Root.State)
	require.Equal(t, "host_lost", got.Root.OutcomeCode)
	require.Zero(t, got.Root.Wake)
	out, err = a.BatchRecoverRoot(ctx, second.Root.RootID, BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out, "a second recoverer must produce no second interruption")
	boom := func(string) (HostLockStatus, error) { return HostStatusDead, errors.New("probe boom") }
	out, err = a.BatchRecoverRoot(ctx, first.Root.RootID, boom)
	require.Error(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out, "a (Dead, error) verdict must write nothing")
	got, err = a.Read(ctx, "owner", first.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, first, got)
	out, err = a.BatchRecoverRoot(ctx, "missing-root", BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out, "an unknown root id must be a no-op")
}
