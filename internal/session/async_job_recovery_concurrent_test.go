package session

// R3A-3 (docs/reviews/2026-09-30-async-phase4-round3.md): recovery holds no
// lock on the dead host while it works. Consequences pinned here: a host being
// recovered still probes dead to every other reader (a crashed driver's session
// can be claimed at once), and N recoverers running truly concurrently produce
// exactly one effect per row. These tests install recoverDeadHostRowSeam, a
// package global, so none of them is parallel.

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// claimMarker records a session_drivers row naming hostID directly.
func claimMarker(t *testing.T, ctx context.Context, q *db.Queries, sessionID, hostID string) {
	t.Helper()
	n, err := q.InsertSessionDriver(ctx, db.InsertSessionDriverParams{SessionID: sessionID, HostID: hostID, Pid: 12345, ClaimedAt: 1700000000})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// holdRecovererAtFirstRow makes the first RecoverDeadHost(hostID) reach its
// per-row step park until release is called; paused closes when it has.
func holdRecovererAtFirstRow(t *testing.T, hostID string) (paused <-chan struct{}, release func()) {
	t.Helper()
	pausedCh := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	recoverDeadHostRowSeam = func(host string, _ db.AsyncJob) {
		if host != hostID {
			return
		}
		first := false
		once.Do(func() { first = true })
		if !first {
			return
		}
		close(pausedCh)
		<-gate
	}
	var releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); recoverDeadHostRowSeam = nil })
	return pausedCh, release
}

// TestRecoverDeadHost_MidRecovery_HostStillProbesDead: a recoverer parked in the
// middle of a dead host's recovery. The host must read as dead to every other
// process: its readers, a claim of a session whose marker names it, and the
// foreign-driver check -- not "alive" until recovery ends.
//
// Revert-check: after the shared probe, take ProbeHost's exclusive lock and
// hold it until RecoverDeadHost returns (the pre-fix behaviour) -> the
// liveness assertion reads alive, ClaimSessionDriver is refused with
// *ErrSessionDrivenElsewhere naming a dead pid.
func TestRecoverDeadHost_MidRecovery_HostStillProbesDead(t *testing.T) {
	stores, q, ctx := nStores(t, 2, "s1", "s2", "owner-1")
	recoverer, other := stores[0], stores[1]
	_, err := other.ensureHost(ctx) // its registration sweep must not recover the fixture host
	require.NoError(t, err)

	const dead = "dead-host-r3a3"
	fabricateDeadHost(t, ctx, q, recoverer.dataDir, dead)
	seedRunningJob(t, ctx, q, "owner-1", "call-1", dead, "", true)
	claimMarker(t, ctx, q, "s1", dead)
	claimMarker(t, ctx, q, "s2", dead)

	paused, release := holdRecovererAtFirstRow(t, dead)
	done := make(chan error, 1)
	go func() {
		_, recErr := recoverer.RecoverDeadHost(ctx, dead, nil)
		done <- recErr
	}()
	<-paused

	require.Equal(t, HostStatusDead, other.HostLiveness(dead), "a host mid-recovery is still dead")
	_, foreign, err := other.ForeignLiveDriver(ctx, "s2")
	require.NoError(t, err)
	require.False(t, foreign, "a crashed driver's marker is not foreign while its host is being recovered")
	require.NoError(t, other.ClaimSessionDriver(ctx, "s1"), "the crashed driver's session is claimable during its host's recovery")
	row, ok := driverRow(t, ctx, other, "s1")
	require.True(t, ok)
	require.Equal(t, other.HostID(), row.HostID)

	release()
	require.NoError(t, <-done)
	got, err := recoverer.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.State)
}

// concurrentRecoverers runs RecoverDeadHost(hostID) on every store at once and
// parks each at the first listed row until ALL of them are there, so every
// recoverer has passed the probe and listed the rows before any writes. A
// recoverer that never gets past the probe (the host reads "alive" to it) leaves
// the others waiting; the wait is bounded and the arrival count is asserted.
func concurrentRecoverers(t *testing.T, ctx context.Context, stores []*AsyncJobStore, hostID string) []RecoveryOutcome {
	t.Helper()
	n := int32(len(stores))
	var (
		arrived  atomic.Int32
		firstKey atomic.Value // tool_call_id of the first listed row
		gate     = make(chan struct{})
	)
	recoverDeadHostRowSeam = func(host string, row db.AsyncJob) {
		if host != hostID {
			return
		}
		firstKey.CompareAndSwap(nil, row.ToolCallID)
		if row.ToolCallID != firstKey.Load() {
			return
		}
		if arrived.Add(1) == n {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
	}
	t.Cleanup(func() { recoverDeadHostRowSeam = nil })

	outcomes := make([]RecoveryOutcome, len(stores))
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, st := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i], errs[i] = st.RecoverDeadHost(ctx, hostID, nil)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, n, arrived.Load(), "every recoverer must get past the probe: a host being recovered still probes dead")
	return outcomes
}

func sumOutcomes(outcomes []RecoveryOutcome) (total RecoveryOutcome, removed int) {
	for _, o := range outcomes {
		total.Interrupted += o.Interrupted
		total.Deleted += o.Deleted
		total.Repended += o.Repended
		if o.HostRemoved {
			removed++
		}
	}
	return total, removed
}

// TestRecoverDeadHost_ConcurrentRecoverers_OneEffectPerRow: six recoverers, all
// past the probe with the same listing, write concurrently. Every row is
// interrupted/deleted/re-pended exactly once, the tallies across recoverers add
// up to the row counts, and a notice is delivered once per row.
//
// Revert-check: make the unannounced-row tally count without checking rows
// affected -> Deleted sums to a multiple of 5.
func TestRecoverDeadHost_ConcurrentRecoverers_OneEffectPerRow(t *testing.T) {
	const recoverers = 6
	stores, q, ctx := nStores(t, recoverers, "owner-1")
	messages := message.NewService(db.New(stores[0].sqlDB))
	const dead = "dead-host-r3a3-race"
	fabricateDeadHost(t, ctx, q, stores[0].dataDir, dead)

	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seedRunningJob(t, ctx, q, "owner-1", id, dead, "", true)
	}
	for _, id := range []string{"u1", "u2", "u3", "t1", "t2"} {
		seedRunningJob(t, ctx, q, "owner-1", id, dead, "", false)
	}
	for _, id := range []string{"t1", "t2"} { // finished before their "started" result: terminal, unannounced
		res, err := stores[0].Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: id, State: "completed", ResultSummary: "ok", Wake: true})
		require.NoError(t, err)
		require.Equal(t, TransitionWon, res.Outcome)
	}
	seedRunningJob(t, ctx, q, "owner-1", "k1", dead, "", true)
	res, err := stores[0].Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "k1", State: "cancelled", NoticeKind: "job_kill",
		ResultSummary: "partial output of k1", Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, res.Outcome)

	total, removed := sumOutcomes(concurrentRecoverers(t, ctx, stores, dead))
	require.Equal(t, 5, total.Interrupted, "each announced running row is interrupted once across all recoverers")
	require.Equal(t, 5, total.Deleted, "each unannounced row (3 running, 2 terminal) is deleted once")
	require.Equal(t, 1, total.Repended, "the unnamed job_kill row is re-pended once")
	require.Zero(t, removed, "rows remain, so the host row stays")

	pulled, err := stores[0].PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 6, "one notice per interrupted row plus the re-pended job_kill row, none twice")
	for _, id := range []string{"u1", "u2", "u3", "t1", "t2"} {
		_, err := stores[0].Get(ctx, "owner-1", id)
		require.ErrorIs(t, err, sql.ErrNoRows, id)
	}
}

// TestRecoverDeadHost_ConcurrentRecoverers_HostRemovedOnce: a host whose every
// row disappears in recovery. Of six concurrent recoverers exactly one deletes
// the async_hosts row and removes the lock file; the rest finish without error.
func TestRecoverDeadHost_ConcurrentRecoverers_HostRemovedOnce(t *testing.T) {
	const recoverers = 6
	stores, q, ctx := nStores(t, recoverers, "owner-1")
	const dead = "dead-host-r3a3-remove"
	fabricateDeadHost(t, ctx, q, stores[0].dataDir, dead)
	for _, id := range []string{"u1", "u2", "u3"} {
		seedRunningJob(t, ctx, q, "owner-1", id, dead, "", false)
	}

	total, removed := sumOutcomes(concurrentRecoverers(t, ctx, stores, dead))
	require.Equal(t, 3, total.Deleted)
	require.Equal(t, 1, removed, "exactly one recoverer removes the host")
	_, statErr := os.Stat(HostLockPath(stores[0].dataDir, dead))
	require.ErrorIs(t, statErr, os.ErrNotExist, "the lock file is removed")
	hosts, err := q.ListAsyncHosts(ctx)
	require.NoError(t, err)
	for _, h := range hosts {
		require.NotEqual(t, dead, h.ID, "the async_hosts row is removed")
	}
}

// TestRecoverDeadHost_StaleListing_KeepsFreshClaimOfSameToolCallID: recoverer A
// listed an unannounced row of the dead host and parks. Recoverer B finishes
// recovery (the row is deleted), and a live host claims the SAME tool_call_id
// afresh. A must not delete that live claim when it resumes: its delete is
// keyed by the claim it listed.
//
// Revert-check: delete by (owner, tool_call_id, announced=0) only, without the
// claim (DeleteUnannouncedAsyncJob) -> the fresh row is gone.
func TestRecoverDeadHost_StaleListing_KeepsFreshClaimOfSameToolCallID(t *testing.T) {
	stores, q, ctx := nStores(t, 2, "owner-1")
	a, b := stores[0], stores[1]
	_, err := b.ensureHost(ctx)
	require.NoError(t, err)

	const dead = "dead-host-r3a3-aba"
	fabricateDeadHost(t, ctx, q, a.dataDir, dead)
	seedRunningJob(t, ctx, q, "owner-1", "reused", dead, "", false)

	paused, release := holdRecovererAtFirstRow(t, dead)
	done := make(chan error, 1)
	go func() {
		_, recErr := a.RecoverDeadHost(ctx, dead, nil)
		done <- recErr
	}()
	<-paused // A has listed "reused" (claim claim-reused) and parks before its delete

	out, err := b.RecoverDeadHost(ctx, dead, nil)
	require.NoError(t, err)
	require.Equal(t, 1, out.Deleted, "B deletes the row A listed")
	fresh, err := b.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "reused", Kind: JobKindCommand, Input: "fresh", ToolName: "bash"})
	require.NoError(t, err)
	require.False(t, fresh.Existing)
	require.NotEqual(t, "claim-reused", fresh.Row.ClaimID)

	release()
	require.NoError(t, <-done)

	got, err := b.Get(ctx, "owner-1", "reused")
	require.NoError(t, err, "the live host's fresh claim must survive the stale recoverer")
	require.Equal(t, fresh.Row.ClaimID, got.ClaimID)
	require.Equal(t, "running", got.State)
}

// TestRecoverDeadHost_StaleListing_AnnouncedRow_KeepsLiveReclaim: recoverer A
// listed an ANNOUNCED running row X of the dead host (claim cX) and parks before
// its Transition. Recoverer B interrupts X (pending, announced), the model's next
// response claims the same tool_call_id again -- X is archived and a new running
// row Y (claim cY, live host) takes the key. When A resumes, its Transition is
// keyed by cX and must lose: Y stays running under cY. Without the claim guard
// ("" resolves to Y's current claim) A would mark the live Y interrupted.
//
// Revert-check: pass ClaimID "" in RecoverDeadHost's TransitionParams -> Y is
// 'interrupted'.
func TestRecoverDeadHost_StaleListing_AnnouncedRow_KeepsLiveReclaim(t *testing.T) {
	stores, q, ctx := nStores(t, 2, "owner-1")
	a, b := stores[0], stores[1]
	_, err := b.ensureHost(ctx)
	require.NoError(t, err)

	const dead = "dead-host-r4a1"
	fabricateDeadHost(t, ctx, q, a.dataDir, dead)
	seedRunningJob(t, ctx, q, "owner-1", "call_0", dead, "", true)

	paused, release := holdRecovererAtFirstRow(t, dead)
	done := make(chan error, 1)
	go func() {
		_, recErr := a.RecoverDeadHost(ctx, dead, nil)
		done <- recErr
	}()
	<-paused // A has listed X (claim-call_0) and parks before its Transition

	out, err := b.RecoverDeadHost(ctx, dead, nil)
	require.NoError(t, err)
	require.Equal(t, 1, out.Interrupted, "B interrupts the row A listed")
	fresh, err := b.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "again", ToolName: "bash"})
	require.NoError(t, err)
	require.False(t, fresh.Existing, "the interrupted, announced row is archived and the key claimed afresh")
	require.NotEqual(t, "claim-call_0", fresh.Row.ClaimID)

	release()
	require.NoError(t, <-done)

	got, err := b.Get(ctx, "owner-1", "call_0")
	require.NoError(t, err)
	require.Equal(t, "running", got.State, "the stale recoverer must not interrupt the live re-claim")
	require.Equal(t, fresh.Row.ClaimID, got.ClaimID)
	require.Equal(t, b.HostID(), got.HostID)
}
