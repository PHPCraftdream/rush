package session_test

// Regression coverage for task #592/P0-1 of the second 2026-08-19
// release-readiness follow-up review: DrainSessionNow used a single
// session-scoped named return (err) as its accumulator, unconditionally
// overwritten by every classified outcome at both dispatch sites. That is
// correct only for a retry of the SAME logical row (see p578's coverage of
// that contract) -- nothing tied the accumulator to row identity, so a
// LATER, DIFFERENT row's clean commit silently cleared an EARLIER row's
// terminal failure, Ack-commit failure, or lease loss, and the whole call
// reported a completed drain over permanently lost or unconfirmed work.
//
// Reproduced by two independent reviews against a real pump and a real
// SQLite DB: row A terminal-fails (row DELETED, work permanently lost) or
// row A's Ack fails (ErrTurnCommitFailed, row still leased, the turn WILL
// re-run) or row A loses its lease mid-execution, row B then commits
// cleanly -- and the pre-#592 code returned a completed drain with a nil
// error in every one of those three cases. Control: row A alone in each
// scenario correctly reports the failure.
//
// The fix is internal/session/run_queue_drain_session.go's rowLedger: a
// row's failure entry is cleared ONLY by a later resolution for that EXACT
// row ID (recordSuccess(rowID)); a different row's success
// (recordSuccess("") or recordSuccess(otherRowID)) never touches it. See
// rowLedger's own doc for why that shape makes the illegal state
// (cross-row clearing) unrepresentable rather than merely checked.
//
// This file pins the contract for BOTH of DrainSessionNow's dispatch paths
// row A's OUTCOME can arrive through (task prompt's explicit requirement):
//
//   - "local execution": row A is leased and run by THIS SAME
//     DrainSessionNow call's own leasing loop -- see the
//     TestDrainSessionNow_LocalExecution_* tests below.
//   - "observed admission": row A is executed by a DIFFERENT, concurrent
//     background tick that THIS DrainSessionNow call loses the admission
//     race to and WAITS on via otherEntry.done, recording the outcome as
//     UNATTRIBUTED (no row ID this call ever leased itself) -- see the
//     TestDrainSessionNow_ObservedAdmission_* tests below, and each one's
//     own "Determinism" note for how the wait is proven to have genuinely
//     happened rather than assumed from timing.
//
// Row B's OWN dispatch mechanism is deliberately NOT pinned in either
// group: whether row B ends up locally leased by this same call (the
// common case for ObservedAdmission_* too, since this call's own retry
// after A's wait resolves is faster than the background tick's next scan
// in practice -- confirmed empirically, not merely assumed, while
// developing this file) or observed via a SECOND wait is immaterial to the
// contract under test -- row A's failure entry (keyed by its own row ID
// for LocalExecution_*, or a synthetic per-occurrence key for
// ObservedAdmission_*'s unattributed case) can only ever be cleared by a
// LATER resolution of THAT EXACT key, never row B's. No test seam exists
// today to force row B specifically through the SECOND-wait path
// deterministically (admission is single-holder per session, so the
// background tick cannot "pre-stage" a claim on row B while row A's
// admission is still held) -- see this file's own revert-check notes in
// the task report for the two-site verification this performs instead:
// reverting the LOCAL-execution success site (run_queue_drain_session.go's
// recordSuccess(leased.ID) call) independently breaks every
// LocalExecution_* test AND every ObservedAdmission_* test (because row B
// resolves locally in practice), while reverting the OBSERVED-ADMISSION
// success site (recordSuccess("")) independently breaks p575's own
// pre-existing WaitedExecution_SuccessIsDrained /
// WaitsForConcurrentBackgroundTick tests, which already exercise that
// exact site directly with a SINGLE row (no local-execution fallback to
// mask it) -- so both sites are independently covered by SOME test in the
// package, even though this file's own ObservedAdmission_* tests happen to
// exercise the LOCAL site for row B rather than the OBSERVED one.
//
// Every combination below must report DrainFailed with the SPECIFIC error
// for row A's outcome -- never DrainComplete, never a bare nil. The sibling
// control (retryable A -> successful retry of the SAME row A) already lives
// in p578_stale_error_test.go's TestDrainSessionNow_SameRowRetrySucceedsClearsErr
// and is not duplicated here.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// twoRowFailThenSucceedCoordinator reports a FIXED outcome for the first
// Coordinator.Run call (row A) and a clean success for every call after
// that (row B, and any further row) -- the inverse of p588's
// twoRowCoordinator, which reports busy from the second call onward. Used
// to force the "row A fails, row B succeeds" sequence deterministically
// through DrainSessionNow's own local-execution loop, without needing to
// simulate external contention.
type twoRowFailThenSucceedCoordinator struct {
	calls   int
	firstFn func() (*any, error)
}

func (c *twoRowFailThenSucceedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls++
	if c.calls == 1 {
		return c.firstFn()
	}
	return nil, nil
}

// TestDrainSessionNow_LocalExecution_TerminalThenSuccess_ReportsFailed is
// the local-execution half of the "terminal A -> success B" contract: row A
// terminal-fails (AlreadyAttempted -- permanently deleted, never retried),
// entirely within THIS DrainSessionNow call's own leasing loop, then row B
// -- a genuinely DIFFERENT durable row -- is leased and committed cleanly
// by the SAME call. The call must report DrainFailed with row A's terminal
// error, never a completed drain: row A's accepted work is gone for good,
// and B succeeding has nothing to do with A's fate.
//
// Revert-check: this is exactly the defect the row ledger's per-row keying
// exists to make unrepresentable -- see the task-report VERBATIM revert
// evidence for the single-site reverts that reproduce it.
func TestDrainSessionNow_LocalExecution_TerminalThenSuccess_ReportsFailed(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p592-local-terminal-then-success")
	termErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("p592: row A already attempted")}
	coord := &twoRowFailThenSucceedCoordinator{firstFn: func() (*any, error) { return nil, termErr }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p592-local-terminal",
	})
	enqueueTwoRows(t, "p592-local-terminal", svc, sess.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "row A (terminal failure) and row B (clean success) must both have been attempted, in the SAME call")
	require.Equal(t, session.DrainFailed, result, "row A's permanent loss must NOT be masked by row B's unrelated success")
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, termErr, "row B's success must not silently replace row A's specific terminal error")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "row A is gone (terminal-failed) and row B is gone (acked) -- nothing left pending for this session")
}

// ackFailingServiceP592 wraps a real session.Service and fails
// AckRunQueueEntry for exactly the first Ack it sees (row A's), letting
// every subsequent Ack (row B's) go to the real implementation -- the same
// pattern as p588's ackFailingServiceP588, duplicated here to keep this
// file's regression self-contained.
type ackFailingServiceP592 struct {
	session.Service
	ackErr    error
	ackedOnce bool
}

func (s *ackFailingServiceP592) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	if !s.ackedOnce {
		s.ackedOnce = true
		return "", s.ackErr
	}
	return s.Service.AckRunQueueEntry(ctx, id, leasedBy)
}

// TestDrainSessionNow_LocalExecution_AckFailureThenSuccess_ReportsFailed is
// the local-execution half of the "Ack failure A -> success B" contract:
// row A's turn runs and would otherwise commit, but its Ack write fails
// (ErrTurnCommitFailed -- row STAYS LEASED, side effects already landed,
// and it WILL be re-run once the lease expires). Row B is then leased and
// committed cleanly by the SAME call. The call must report DrainFailed with
// ErrTurnCommitFailed, never a completed drain: telling the operator this
// run succeeded would hide that row A's turn is going to execute a SECOND
// time.
func TestDrainSessionNow_LocalExecution_AckFailureThenSuccess_ReportsFailed(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p592-local-ack-then-success")
	failing := &ackFailingServiceP592{Service: svc, ackErr: errors.New("p592: disk on fire during row A's ack")}
	coord := &twoRowFailThenSucceedCoordinator{firstFn: func() (*any, error) { return nil, nil }}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p592-local-ack",
	})
	enqueueTwoRows(t, "p592-local-ack", failing, sess.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, 2, coord.calls, "row A's turn (Ack fails after) and row B (clean success) must both have been attempted, in the SAME call")
	require.Equal(t, session.DrainFailed, result, "row A's uncommitted turn must NOT be masked by row B's unrelated success")
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, session.ErrTurnCommitFailed, "row B's success must not silently replace row A's specific Ack-failure sentinel")

	// Row A must still be leased (recoverable via lease expiry, will re-run
	// -- see ErrTurnCommitFailed's own doc), row B must be gone (acked).
	stale, err := svc.ListStaleLeasedRunQueueEntries(context.Background(), time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.Len(t, stale, 1, "row A must still exist and still be leased so lease-expiry recovery can re-run it")
	require.Contains(t, stale[0].CallData, "row-A")
}

// leaseStealingServiceP592 wraps a real session.Service and makes
// RenewRunQueueLease fail (reassigned, ok=false, err=nil) for exactly ONE
// specific row ID -- row A's -- while renewing every other row (row B's)
// normally through the real implementation. Unlike p575's
// leaseStealingService (which fails EVERY renewal unconditionally, fine for
// a single-row test), this file's scenario needs row B's own turn to
// complete normally in the SAME call, which requires row B's lease to
// renew successfully if its turn runs long enough to need one.
type leaseStealingServiceP592 struct {
	session.Service
	victimID string
}

func (s *leaseStealingServiceP592) RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error) {
	if id == s.victimID {
		return false, nil
	}
	return s.Service.RenewRunQueueLease(ctx, id, leasedBy, newExpiresAt)
}

// gatedThenSucceedCoordinator runs row A's call through a real, external
// gate (so a test can hold it open long enough for the lease-renewal loop
// to attempt -- and, via leaseStealingServiceP592, fail -- a renewal), then
// reports a clean success for every subsequent call (row B, and any
// further row).
type gatedThenSucceedCoordinator struct {
	calls   int
	started chan struct{}
	release chan struct{}
}

func (c *gatedThenSucceedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls++
	if c.calls == 1 {
		select {
		case c.started <- struct{}{}:
		default:
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return nil, nil
	}
	return nil, nil
}

// TestDrainSessionNow_LocalExecution_LeaseLossThenSuccess_ReportsFailed is
// the local-execution half of the "lease loss A -> success B" contract: row
// A's lease is reassigned to a different owner mid-execution (this call's
// renewal loop observes the reassignment and cancels its own execCtx --
// see errLeaseLost's own doc), and row A's turn returns errLeaseLost. Row B
// -- a genuinely different row -- is then leased and committed cleanly by
// the SAME call. The call must report DrainFailed with errLeaseLost's
// message, never a completed drain: row A's fate now belongs to whoever
// re-leased it, and this process learned nothing about it that row B's
// unrelated success could possibly resolve.
func TestDrainSessionNow_LocalExecution_LeaseLossThenSuccess_ReportsFailed(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p592-local-lease-then-success")

	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p592-local-lease-row-A", sess.ID, callDataA))

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "row A must be the only pending entry at this point, so its ID can be captured unambiguously")
	rowAID := pending[0].ID

	stealing := &leaseStealingServiceP592{Service: svc, victimID: rowAID}
	coord := &gatedThenSucceedCoordinator{started: make(chan struct{}, 1), release: make(chan struct{})}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       stealing,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p592-local-lease",
		// A short TTL makes the renewal loop attempt its first (rejected,
		// for row A specifically) renewal quickly instead of waiting out
		// the 30s production TTL/3, mirroring p575's own lease-loss test.
		TestLeaseTTL: 60 * time.Millisecond,
	})

	callDataB, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-B"})
	require.NoError(t, err)
	require.NoError(t, stealing.EnqueueRunQueueEntry(context.Background(), "p592-local-lease-row-B", sess.ID, callDataB))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Confirm row A's turn genuinely started before letting the renewal
	// loop's own watchdog/timing drive it to lease loss and execCancel() --
	// the gated coordinator's Run() call observes ctx.Done() and returns on
	// its own once that happens, so nothing needs to be sent on
	// coord.release for row A.
	select {
	case <-coord.started:
	case <-time.After(5 * time.Second):
		t.Fatal("row A's turn never started -- test setup is broken, proves nothing about the race under test")
	}

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return -- row A's lease loss or row B's execution never resolved")
	}

	require.Equal(t, 2, coord.calls, "row A (lease loss) and row B (clean success) must both have been attempted, in the SAME call")
	require.Equal(t, session.DrainFailed, result, "row A's lease loss must NOT be masked by row B's unrelated success")
	require.Error(t, drainErr)
	require.ErrorContains(t, drainErr, "lost lease ownership", "row B's success must not silently replace row A's specific lease-loss outcome")
}

// sequentialGatedCoordinator is gatedCoordinator's multi-call sibling: each
// successive Coordinator.Run call parks on its OWN gate, taken from a
// pre-supplied ordered list, so a test can control exactly what EACH of
// several sequential background-tick executions returns -- not just the
// first. Used by the observed-admission variants below, where BOTH row A
// and row B are executed by the pump's own background tick (never by
// DrainSessionNow's local-lease branch), so the row-identity contract is
// exercised entirely through the wait-on-admissionEntry path for BOTH rows,
// not just one of them.
type sequentialGatedCoordinator struct {
	calls   int
	started chan struct{}
	gates   []chan error // gates[i] is call i+1's release channel
}

func newSequentialGatedCoordinator(n int) *sequentialGatedCoordinator {
	c := &sequentialGatedCoordinator{started: make(chan struct{}, n)}
	for i := 0; i < n; i++ {
		c.gates = append(c.gates, make(chan error))
	}
	return c
}

func (c *sequentialGatedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	idx := c.calls
	c.calls++
	select {
	case c.started <- struct{}{}:
	default:
	}
	if idx >= len(c.gates) {
		return nil, nil
	}
	select {
	case err := <-c.gates[idx]:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestDrainSessionNow_ObservedAdmission_TerminalThenSuccess_ReportsFailed
// is the observed-admission half of the "terminal A -> success B" contract:
// the pump's own background tick executes row A (terminal-fails,
// AlreadyAttempted) while a concurrent DrainSessionNow call has already
// lost the admission race and is waiting on A's admissionEntry. Once A
// resolves, THIS call's wait branch classifies the terminal failure and
// records it as UNATTRIBUTED (this call never leased row A itself -- see
// recordUnattributed's doc), then loops back to check for more pending
// work. Row B is then leased and committed cleanly -- by EITHER this same
// DrainSessionNow call's own local-execution branch (the common case,
// since it retries admission essentially immediately after A's wait
// resolves, usually beating the background tick's own next scan) OR a
// second observed wait if the tick gets there first; both are valid
// resolutions of the SAME contract, since the assertion under test is
// specifically that row A's UNATTRIBUTED failure entry is never cleared by
// ANY other row's success, local or observed (recordSuccess only deletes
// its OWN rowID key -- see rowLedger's own doc). The call must report
// DrainFailed with row A's terminal error, never a completed drain.
//
// Determinism: TestAfterAdmissionRefusal proves THIS call was genuinely
// refused admission and forced to wait for row A's outcome specifically
// (rather than assuming it from timing) -- if that refusal never happened,
// this call would have leased and executed row A itself, which is a
// DIFFERENT contract (already covered by the LocalExecution sibling test
// above) and would make this test's "observed" framing false.
func TestDrainSessionNow_ObservedAdmission_TerminalThenSuccess_ReportsFailed(t *testing.T) {
	sess, svc := setupTestSession(t, "p592-observed-terminal-then-success")
	gate := newSequentialGatedCoordinator(1)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-p592-observed-terminal",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})
	enqueueTwoRows(t, "p592-observed-terminal", svc, sess.ID)

	var refusals atomic.Int64
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			refusals.Add(1)
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing row A -- test setup is broken, proves nothing about the race under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give DrainSessionNow time to lose the admission race for row A and
	// enter its wait branch before resolving row A's execution. Row B (the
	// second and any further Coordinator.Run call) is not gated at all --
	// see sequentialGatedCoordinator's doc -- and always succeeds, so no
	// second release is needed once the background tick reaches it.
	time.Sleep(150 * time.Millisecond)
	termErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("p592: row A already attempted (observed)")}
	gate.gates[0] <- termErr

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return -- row A's observed terminal failure or row B's observed success never resolved")
	}

	require.GreaterOrEqual(t, gate.calls, 2, "both row A (terminal failure) and row B (clean success) must have been attempted by the background tick")
	require.GreaterOrEqual(t, refusals.Load(), int64(1), "this call must have been refused admission and forced to WAIT for row A's outcome -- otherwise this test exercises the local-execution path for row A instead of the observed-admission path it claims to, proving nothing about the contract under test (row B may resolve via either path -- see this test's doc)")
	require.Equal(t, session.DrainFailed, result, "row A's observed terminal failure must NOT be masked by row B's unrelated observed success")
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, termErr, "row B's observed success must not silently replace row A's specific terminal error")
}

// ackFailOnceService wraps a real session.Service and fails EXACTLY the
// first AckRunQueueEntry call it observes, letting every subsequent Ack go
// to the real implementation -- reused (renamed to keep this file
// self-contained) for the observed-admission Ack-failure variant, where the
// SAME service instance must be shared by the pump's background tick
// executing BOTH row A (fails) and row B (succeeds).
type ackFailOnceService struct {
	session.Service
	ackErr    error
	ackedOnce bool
}

func (s *ackFailOnceService) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	if !s.ackedOnce {
		s.ackedOnce = true
		return "", s.ackErr
	}
	return s.Service.AckRunQueueEntry(ctx, id, leasedBy)
}

// TestDrainSessionNow_ObservedAdmission_AckFailureThenSuccess_ReportsFailed
// is the observed-admission half of the "Ack failure A -> success B"
// contract: the pump's own background tick runs row A's turn to completion,
// but its Ack write fails (ErrTurnCommitFailed -- row A STAYS LEASED, WILL
// re-run after lease expiry), while a concurrent DrainSessionNow call has
// lost the admission race and is waiting on A's admissionEntry. Once A
// resolves, this call's wait branch records the Ack failure as
// UNATTRIBUTED, then loops back and either locally leases row B itself or
// observes the tick execute it -- see the terminal-failure sibling test's
// doc for why both are valid resolutions of the same contract. The call
// must report DrainFailed with ErrTurnCommitFailed, never a completed
// drain.
//
// Determinism: see the terminal-failure sibling test's doc for why the
// refusals counter (not timing) is what proves row A's outcome was
// genuinely OBSERVED via a wait, not locally leased by this call.
func TestDrainSessionNow_ObservedAdmission_AckFailureThenSuccess_ReportsFailed(t *testing.T) {
	sess, svc := setupTestSession(t, "p592-observed-ack-then-success")
	failing := &ackFailOnceService{Service: svc, ackErr: errors.New("p592: disk on fire during row A's observed ack")}
	gate := newSequentialGatedCoordinator(1)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-p592-observed-ack",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})
	enqueueTwoRows(t, "p592-observed-ack", failing, sess.ID)

	var refusals atomic.Int64
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			refusals.Add(1)
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing row A -- test setup is broken, proves nothing about the race under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	time.Sleep(150 * time.Millisecond)
	gate.gates[0] <- nil // row A's turn itself succeeds; the Ack write is what fails

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return -- row A's observed Ack failure or row B's observed success never resolved")
	}

	require.GreaterOrEqual(t, gate.calls, 2, "both row A (Ack fails) and row B (clean success) must have been attempted by the background tick")
	require.GreaterOrEqual(t, refusals.Load(), int64(1), "this call must have been refused admission and forced to WAIT for row A's outcome -- otherwise this test exercises the local-execution path for row A instead of the observed-admission path it claims to")
	require.Equal(t, session.DrainFailed, result, "row A's observed Ack failure must NOT be masked by row B's unrelated observed success")
	require.Error(t, drainErr)
	require.ErrorIs(t, drainErr, session.ErrTurnCommitFailed, "row B's observed success must not silently replace row A's specific Ack-failure sentinel")
}

// leaseStealingServiceObserved wraps a real session.Service and fails
// RenewRunQueueLease for exactly ONE specific row ID (row A's, captured
// before the pump ever leases anything), letting row B's own renewal (if
// its turn runs long enough to need one) go to the real implementation --
// the observed-admission counterpart to leaseStealingServiceP592 above.
type leaseStealingServiceObserved struct {
	session.Service
	victimID string
}

func (s *leaseStealingServiceObserved) RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error) {
	if id == s.victimID {
		return false, nil
	}
	return s.Service.RenewRunQueueLease(ctx, id, leasedBy, newExpiresAt)
}

// TestDrainSessionNow_ObservedAdmission_LeaseLossThenSuccess_ReportsFailed
// is the observed-admission half of the "lease loss A -> success B"
// contract: the pump's own background tick starts row A's turn, but row
// A's lease is reassigned mid-execution (this scenario's renewal loop
// observes the reassignment and cancels execCtx -- see errLeaseLost's own
// doc), while a concurrent DrainSessionNow call has lost the admission race
// and is waiting on A's admissionEntry. Once A resolves (to errLeaseLost),
// this call's wait branch records it as UNATTRIBUTED, then loops back and
// either locally leases row B itself or observes the tick execute it -- see
// the terminal-failure sibling test's doc for why both are valid
// resolutions of the same contract. The call must report DrainFailed with
// errLeaseLost's message, never a completed drain: row A's fate now
// belongs to whoever re-leased it, and row B's unrelated success resolves
// nothing about it.
//
// Determinism: see the terminal-failure sibling test's doc for why the
// refusals counter (not timing) is what proves row A's outcome was
// genuinely OBSERVED via a wait, not locally leased by this call. Row A
// parks on gates[0] and is never manually released -- its own renewal loop
// drives it to lease loss and execCancel(), which the gated Run() observes
// via ctx.Done() and returns on its own, exactly like p575's
// WaitedExecution_LeaseLossIsSurfaced.
func TestDrainSessionNow_ObservedAdmission_LeaseLossThenSuccess_ReportsFailed(t *testing.T) {
	sess, svc := setupTestSession(t, "p592-observed-lease-then-success")

	callDataA, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-A"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p592-observed-lease-row-A", sess.ID, callDataA))

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "row A must be the only pending entry at this point, so its ID can be captured unambiguously")
	rowAID := pending[0].ID

	stealing := &leaseStealingServiceObserved{Service: svc, victimID: rowAID}
	gate := newSequentialGatedCoordinator(1)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       stealing,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-p592-observed-lease",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
		TestLeaseTTL:   60 * time.Millisecond,
	})

	callDataB, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "row-B"})
	require.NoError(t, err)
	require.NoError(t, stealing.EnqueueRunQueueEntry(context.Background(), "p592-observed-lease-row-B", sess.ID, callDataB))

	var refusals atomic.Int64
	pump.SetTestAfterAdmissionRefusalForTest(func(sessionID string) {
		if sessionID == sess.ID {
			refusals.Add(1)
		}
	})

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing row A -- test setup is broken, proves nothing about the race under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return -- row A's observed lease loss or row B's observed success never resolved")
	}

	require.GreaterOrEqual(t, gate.calls, 2, "both row A (lease loss) and row B (clean success) must have been attempted by the background tick")
	require.GreaterOrEqual(t, refusals.Load(), int64(1), "this call must have been refused admission and forced to WAIT for row A's outcome -- otherwise this test exercises the local-execution path for row A instead of the observed-admission path it claims to")
	require.Equal(t, session.DrainFailed, result, "row A's observed lease loss must NOT be masked by row B's unrelated observed success")
	require.Error(t, drainErr)
	require.ErrorContains(t, drainErr, "lost lease ownership", "row B's observed success must not silently replace row A's specific lease-loss outcome")
}
