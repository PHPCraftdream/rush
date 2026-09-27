package agent

// Core workLedger coverage: plain-job lifecycle (Start/finish/acknowledged/
// abort/cancel/close, ported from the former async_job_registry_test.go) plus
// the phase-1 CAS/idempotency/ack-gate properties that did not exist as
// separate tests before (docs/plans/2026-09-27-async-phase1-spec.md §4.2).

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// drainCompletions collects every completion sent on ch, non-blockingly.
func drainCompletions(ch chan AsyncCompletion) []AsyncCompletion {
	got := make([]AsyncCompletion, 0, 4)
	for {
		select {
		case c := <-ch:
			got = append(got, c)
		default:
			return got
		}
	}
}

// TestWorkLedger_WaitsForPersistedToolResult ports
// TestAsyncJobRegistryWaitsForPersistedToolResult: delivery waits for
// acknowledged, and the ready queue is FIFO.
func TestWorkLedger_WaitsForPersistedToolResult(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, existing, err := l.Start("session", "call", "", "bash", "", true, nil)
	require.NoError(t, err)
	require.False(t, existing)
	want := AsyncCompletion{SessionID: "session", ToolCallID: "call", ToolName: "bash", Content: "done"}
	l.finish("session", "call", jobResult{content: "done"})

	l.mu.Lock()
	ready, pending := len(l.bySession["session"].ready), len(l.bySession["session"].jobs)
	l.mu.Unlock()
	require.Zero(t, ready)
	require.Equal(t, 1, pending)

	l.acknowledged("session", "call")
	got, ok, err := l.next(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, got)
	_, ok, err = l.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestWorkLedger_WebCallbackExactlyOnce ports
// TestAsyncJobRegistryWebCallbackExactlyOnce: a repeated finish for an
// already-terminal job is a no-op.
func TestWorkLedger_WebCallbackExactlyOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	l := newWorkLedger(func(got AsyncCompletion) {
		require.Equal(t, "call", got.ToolCallID)
		calls.Add(1)
	})
	_, _, err := l.Start("session", "call", "", "bash", "", false, nil)
	require.NoError(t, err)
	l.acknowledged("session", "call")
	l.finish("session", "call", jobResult{})
	l.finish("session", "call", jobResult{})
	require.EqualValues(t, 1, calls.Load())
	_, ok, err := l.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestWorkLedger_ConcurrentFinishAndAcknowledge ports
// TestAsyncJobRegistryConcurrentFinishAndAcknowledge: whichever of
// finish/acknowledged runs last is what delivers, exactly once.
func TestWorkLedger_ConcurrentFinishAndAcknowledge(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("session", "call", "", "bash", "", true, nil)
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Go(func() { l.finish("session", "call", jobResult{}) })
	wg.Go(func() { l.acknowledged("session", "call") })
	wg.Wait()
	_, ok, err := l.next(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = l.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
}

// TestWorkLedger_CancelAndClose ports TestAsyncJobRegistryCancelAndClose:
// close cancels every job's executor context and refuses further Start.
func TestWorkLedger_CancelAndClose(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := l.Start("session", "call", "", "bash", "", true, cancel)
	require.NoError(t, err)
	waitCtx, stopWaiting := context.WithCancel(t.Context())
	stopWaiting()
	_, _, err = l.next(waitCtx, "session")
	require.ErrorIs(t, err, context.Canceled)
	l.close()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	_, ok, err := l.next(t.Context(), "session")
	require.NoError(t, err)
	require.False(t, ok)
	_, _, err = l.Start("session", "new-call", "", "bash", "", true, nil)
	require.ErrorContains(t, err, "closed")
}

// TestWorkLedger_StartIsIdempotentPerToolCallID pins #1038 (spec §4.2d): a
// repeated Start for the same (owner, toolCallID) returns the SAME job with
// existing=true, never a second one -- the caller must not run its executor
// twice for one tool call.
//
// Revert-check performed: reverted Start to return an error on a duplicate
// key (today's async_job_registry.go shape: `fmt.Errorf("async job %s is
// already running", toolCallID)`) instead of `(existing, true, nil)`. Ran
// `go test -run TestWorkLedger_StartIsIdempotentPerToolCallID`: FAILED at
// `require.False(t, existing1)`'s sibling assertion --
// `require.NoError(t, err)` on the second Start failed with "async job call
// is already running", exactly the class of failure the spec's revert-check
// describes. Restored the idempotent form, `go build ./...` and the test
// re-run both passed.
func TestWorkLedger_StartIsIdempotentPerToolCallID(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	job1, existing1, err := l.Start("owner", "call", "", "bash", "", true, nil)
	require.NoError(t, err)
	require.False(t, existing1)
	job2, existing2, err := l.Start("owner", "call", "", "bash", "", true, nil)
	require.NoError(t, err)
	require.True(t, existing2)
	require.Same(t, job1, job2, "a repeated Start for the same key must return the SAME job")
}

// A provider that reuses a tool call id for a DIFFERENT command must get an
// error, not a "started" reply for a command that never runs.
//
// Revert check: drop the input comparison in Start — the second Start
// returns existing=true with no error.
func TestWorkLedger_ReusedCallIDWithDifferentInputIsRefused(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	_, _, err := l.Start("owner", "call_0", `{"command":"go test"}`, "bash", "", true, nil)
	require.NoError(t, err)
	_, existing, err := l.Start("owner", "call_0", `{"command":"rm -rf build"}`, "bash", "", true, nil)
	require.Error(t, err, "a different command reusing a live id must not be joined silently")
	require.False(t, existing)
}

// TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce pins the ack-gate for
// the delegation path (spec §4.2c): arming a delegation whose child scope is
// ALREADY drained must still not deliver until Announce (acknowledged) has
// run. The former test helper (subagent_outcome_test.go's parkDelegation)
// always acknowledged before parking, so this exact ordering was never
// checked in isolation before.
//
// Revert-check performed: removed the `|| !job.announced` clause from
// deliverLocked's guard. Ran this test: FAILED ("must not deliver before
// announce") because armDelegation's synchronous recheckChild call now
// delivered immediately. Restored the guard; `go build ./...` and the test
// re-run both passed.
func TestWorkLedger_DelegationNeverDeliveredBeforeAnnounce(t *testing.T) {
	t.Parallel()
	delivered := make(chan AsyncCompletion, 1)
	l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	_, _, err := l.Start("parent", "call", "", AgentToolName, "child", false, nil)
	require.NoError(t, err)

	// childScopeDrained is trivially true here: l.coord is nil, and the
	// child owns no jobs of its own, so arming should be release-ready
	// immediately -- announce is the only thing withholding delivery.
	l.armDelegation("parent", "call", jobResult{content: "child yielded: done"})

	select {
	case <-delivered:
		t.Fatal("must not deliver before the started tool result is announced")
	default:
	}
	require.True(t, l.running("parent"), "the job must still be present, waiting for announce")

	l.acknowledged("parent", "call")
	select {
	case got := <-delivered:
		require.Equal(t, "child yielded: done", got.Content)
	default:
		t.Fatal("must deliver once announced")
	}
}

// TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome pins ASYNC-03
// (spec §4.2a): a race between two finish outcomes and a cancel for the SAME
// job yields exactly one delivered outcome, never zero, never two.
//
// A delegation-shaped job (non-empty childSession) is used deliberately: a
// PLAIN job's cancellation is a silent drop with no delivery at all (kept
// byte-for-byte identical to today's asyncJobRegistry.cancelSession, see
// cancelSession's doc and TestSubAgentOutcome_CancelSurvivesFinishedChildTurn
// in work_ledger_delegation_test.go), so racing cancel against finish for a
// plain job would make "exactly one" depend on which side wins -- not a
// property of the CAS, but of that separately-preserved behavior. A
// delegation's cancellation DOES go through the same delivery path as its
// normal completion, so every one of the three racers can independently
// produce the single winning outcome, which is what actually exercises
// transitionToTerminal's CAS deterministically.
//
// Run repeatedly (not just once) because which of the three goroutines wins
// the mutex is scheduler-dependent; a single trial could pass by luck even
// with a broken CAS.
//
// Revert-check performed: reverted cancelSession to today's
// async_job_registry.go shape (snapshot s.jobs, replace it with a fresh
// map, cancel contexts from the snapshot afterward, no
// transitionToTerminal/deliverLocked call at all). Ran this test: FAILED
// within the first few of the 30 iterations with `require.Len(t, got, 1)`
// seeing 0 deliveries (cancel's goroutine had swapped the map before finish
// could find its row) on some iterations, and did not fail others --
// exactly the nondeterministic "0 or 2+ deliveries" signature the spec's
// revert-check describes. Restored the per-job transitionToTerminal-based
// cancelSession; `go build ./...` and 30/30 re-run iterations passed.
func TestWorkLedger_ConcurrentTerminalRaceYieldsExactlyOneOutcome(t *testing.T) {
	t.Parallel()
	for i := 0; i < 30; i++ {
		delivered := make(chan AsyncCompletion, 8)
		l := newWorkLedger(func(c AsyncCompletion) { delivered <- c })
		_, _, err := l.Start("owner", "call", "", AgentToolName, "child", false, nil)
		require.NoError(t, err)
		l.acknowledged("owner", "call")

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			<-start
			l.finish("owner", "call", jobResult{content: "ok"})
		}()
		go func() {
			defer wg.Done()
			<-start
			l.finish("owner", "call", jobResult{content: "boom", isError: true})
		}()
		go func() {
			defer wg.Done()
			<-start
			l.cancelSession("owner")
		}()
		close(start)
		wg.Wait()

		got := drainCompletions(delivered)
		require.Len(t, got, 1, "exactly one terminal outcome must be delivered per race (iteration %d)", i)
		require.False(t, l.running("owner"), "the job must be fully resolved, not left dangling (iteration %d)", i)
	}
}
