package agent

// Core workLedger coverage: plain-job lifecycle (Start/finish/acknowledged/
// abort/cancel/close, ported from the former async_job_registry_test.go) plus
// the phase-1 CAS/idempotency/ack-gate properties that did not exist as
// separate tests before (docs/plans/2026-09-27-async-phase1-spec.md §4.2).

import (
	"context"
	"fmt"
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
	l.store = newTestAsyncJobStore(t)
	_, existing, err := l.Start("session", "call", "", "bash", "", true, false, nil, nil)
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
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session", "call", "", "bash", "", false, false, nil, nil)
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
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session", "call", "", "bash", "", true, false, nil, nil)
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
//
// Phase-4 step 2 deliberately changes one part of this test's old
// assertion: close() no longer removes the job from memory or transitions
// it (doc sec.3.1/3.7, "graceful exit = crash") -- the row stays 'running'
// for the next host to recover, so l.next() must NOT be called after
// close() expecting a not-ok/empty result (it would block forever waiting
// for a delivery that will never come). This test now asserts the executor
// IS cancelled and the job REMAINS tracked (l.running still true).
//
// Revert-check performed: reverted close() to its pre-step-2 form (call
// cancelSession per owner) -- this test's OLD assertion block (l.next
// returning not-ok) passed again, but TestWorkLedger_
// ShutdownCausedCancellationLeavesRowRunningWritesNoNotice (work_ledger_
// durable_test.go) FAILED (the row committed to a terminal state instead of
// staying 'running'), proving the two tests pin opposite, mutually
// exclusive behaviors and confirming THIS test's assertion is the one that
// had to change. Restored close(); re-ran both, passed.
func TestWorkLedger_CancelAndClose(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := l.Start("session", "call", "", "bash", "", true, false, nil, cancel)
	require.NoError(t, err)
	waitCtx, stopWaiting := context.WithCancel(t.Context())
	stopWaiting()
	_, _, err = l.next(waitCtx, "session")
	require.ErrorIs(t, err, context.Canceled)
	l.close()
	require.ErrorIs(t, ctx.Err(), context.Canceled, "close must cancel every job's executor context")
	require.True(t, l.running("session"), "the job must stay tracked (DB row stays 'running' for recovery), not be dropped")
	_, _, err = l.Start("session", "new-call", "", "bash", "", true, false, nil, nil)
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
	l.store = newTestAsyncJobStore(t)
	job1, existing1, err := l.Start("owner", "call", "", "bash", "", true, false, nil, nil)
	require.NoError(t, err)
	require.False(t, existing1)
	job2, existing2, err := l.Start("owner", "call", "", "bash", "", true, false, nil, nil)
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
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("owner", "call_0", `{"command":"go test"}`, "bash", "", true, false, nil, nil)
	require.NoError(t, err)
	_, existing, err := l.Start("owner", "call_0", `{"command":"rm -rf build"}`, "bash", "", true, false, nil, nil)
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
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("parent", "call", "", AgentToolName, "child", false, false, nil, nil)
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
		l.store = newTestAsyncJobStore(t)
		_, _, err := l.Start("owner", "call", "", AgentToolName, "child", false, false, nil, nil)
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

// TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone pins phase 2 §4.4: a
// sync job's outcome is delivered ONLY through awaitSync/job.done, never
// through the CLI ready queue or the web onWebDone callback.
// Revert-check performed: removed the `if job.sync {...}` short-circuit at
// the top of deliverLocked's body -- this test FAILED (onWebDone was
// invoked, len(s.ready) became 1 for a cli=true job). Restored the
// short-circuit; re-ran, passed.
func TestWorkLedger_SyncJobBypassesReadyQueueAndWebDone(t *testing.T) {
	t.Parallel()
	var webDoneCalls int
	l := newWorkLedger(func(AsyncCompletion) { webDoneCalls++ })
	l.store = newTestAsyncJobStore(t)
	job, existing, err := l.Start("session", "call", "", "bash", "", true, true, nil, nil)
	require.NoError(t, err)
	require.False(t, existing)
	require.NotNil(t, job.done)

	l.finish("session", "call", jobResult{content: "sync result"})

	select {
	case <-job.done:
	default:
		t.Fatal("job.done must be closed once the sync job is delivered")
	}
	l.mu.Lock()
	ready := len(l.bySession["session"].ready)
	l.mu.Unlock()
	require.Zero(t, ready, "a sync job must never be queued for CLI drain")
	require.Zero(t, webDoneCalls, "a sync job must never invoke onWebDone")

	result, err := l.awaitSync(t.Context(), job)
	require.NoError(t, err)
	require.Equal(t, "sync result", result.content)
}

// TestWorkLedger_ConsumeNoticeKeepsMapAndKeysInLockstep pins the orchestrator
// review's P2 finding: consumeNotice used to delete only from l.noticedBefore,
// leaving the key in l.noticedBeforeKeys forever -- N record+consume cycles
// in a long-lived web process grew the slice unboundedly even though the map
// itself never exceeded maxNoticedBefore (eviction there only fires once the
// MAP is over the bound, so it never observed the orphaned slice growth).
//
// Revert-check performed: reverted consumeNotice to
// `delete(l.noticedBefore, key)` only (no slice removal) -- this test FAILED
// (noticedBeforeKeys had length 50 instead of 0). Restored the fix; re-ran,
// passed.
func TestWorkLedger_ConsumeNoticeKeepsMapAndKeysInLockstep(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	const n = 50
	for i := 0; i < n; i++ {
		job := jobIdentity{owner: "owner", toolCallID: fmt.Sprintf("call-%d", i)}
		l.recordNotice(job, fmt.Sprintf("msg-%d", i))
		l.consumeNotice(job)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	require.Empty(t, l.noticedBefore, "every recorded notice was consumed; the map must end up empty")
	require.Empty(t, l.noticedBeforeKeys, "every recorded notice was consumed; the key-order slice must end up empty too")
}

// TestWorkLedger_NoticeReRecordedAfterConsumeDoesNotDuplicateKey pins the
// second half of the same finding: recording the SAME job identity again
// after it was consumed must not leave a duplicate entry in
// noticedBeforeKeys -- a duplicate let eviction pop a stale occurrence of the
// key and delete the FRESH map entry that key now collides with, silently
// losing a just-recorded notice's idempotency guard.
//
// Revert-check performed: same revert as above (consumeNotice not touching
// noticedBeforeKeys) -- this test FAILED (len(noticedBeforeKeys) == 2:
// "call-1" appeared once from the first recordNotice, and again from the
// second, since consumeNotice never removed the first occurrence). Restored
// the fix; re-ran, passed.
func TestWorkLedger_NoticeReRecordedAfterConsumeDoesNotDuplicateKey(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	job := jobIdentity{owner: "owner", toolCallID: "call-1"}
	l.recordNotice(job, "msg-1")
	l.consumeNotice(job)
	l.recordNotice(job, "msg-2")

	l.mu.Lock()
	defer l.mu.Unlock()
	require.Len(t, l.noticedBeforeKeys, 1, "re-recording after consume must not leave a duplicate key")
	require.Equal(t, "msg-2", l.noticedBefore[noticedBeforeKey(job.owner, job.toolCallID)])
}
