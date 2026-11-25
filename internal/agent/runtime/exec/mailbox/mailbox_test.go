package mailbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// Stage 1 (unwired) unit tests for the Mailbox type itself, per
// docs/plans/2026-08-04-session-owner-Mailbox-design.md §3-§5. These tests
// exercise Mailbox in isolation — no sessionAgent wiring, since nothing
// calls into Mailbox yet.

func TestMailbox_Submit_BecomesOwnerWhenIdle(t *testing.T) {
	mb := &Mailbox{}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "hello"}
	cancelCalled := false
	cancel := func() { cancelCalled = true }

	becomeOwner, Epoch := mb.Submit(call, cancel)

	require.True(t, becomeOwner, "first Submit on an idle Mailbox must become owner")
	require.NotZero(t, Epoch, "a granted ownership era must have a non-zero Epoch")
	require.Equal(t, MbOwned, mb.State)
	require.Empty(t, mb.Submitted, "submitted Queue must stay empty when becoming owner directly")
	require.False(t, cancelCalled, "Submit must not invoke the dispatcher cancel func itself")
}

func TestMailbox_Submit_QueuesWhenAlreadyOwned(t *testing.T) {
	mb := &Mailbox{}
	first := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "first"}
	second := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "second"}

	becomeOwner1, epoch1 := mb.Submit(first, func() {})
	require.True(t, becomeOwner1)
	require.NotZero(t, epoch1)

	becomeOwner2, epoch2 := mb.Submit(second, func() {})
	require.False(t, becomeOwner2, "Submit while owned must Queue, not become owner")
	require.Zero(t, epoch2, "Epoch is meaningless when the caller does not become owner")
	require.Equal(t, MbOwned, mb.State, "state must remain owned")
	require.Len(t, mb.Submitted, 1)
	require.Equal(t, second, mb.Submitted[0])
}

// TestMailbox_Submit_DurableCallSkipsQueueWhenAlreadyOwned is the direct
// unit-level regression test for P0-1
// (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
// a call with FromDurableQueue=true must NOT be appended to mb.Submitted
// when the Mailbox is already owned — the durable row itself is the retry
// path (RunQueuePump re-leases it after ErrCallQueuedNotExecuted's backoff),
// so queuing a second copy here would let both the live owner (draining
// mb.Submitted) and the pump (re-leasing the durable row) execute the same
// logical request independently.
//
// The rush-delegated fix's own test (p0_1_durable_double_execution_test.go
// in internal/session) exercises this indirectly through a mocked
// Coordinator that never touches the real Mailbox — verified by hand that
// it still passes with the `if !call.FromDurableQueue` guard removed, i.e.
// it does not actually prove the fix. This test targets the exact line
// instead.
//
// REVERT CHECK: remove the `if !call.FromDurableQueue` guard in Submit()
// (restore unconditional `mb.Submitted = append(mb.Submitted, call)`), this
// test fails (submitted becomes length 1 instead of 0); restore the guard,
// it passes again.
func TestMailbox_Submit_DurableCallSkipsQueueWhenAlreadyOwned(t *testing.T) {
	mb := &Mailbox{}
	first := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "live owner's own turn"}
	durable := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "durable retry", FromDurableQueue: true}

	becomeOwner1, epoch1 := mb.Submit(first, func() {})
	require.True(t, becomeOwner1)
	require.NotZero(t, epoch1)

	becomeOwner2, epoch2 := mb.Submit(durable, func() {})
	require.False(t, becomeOwner2, "Submit while owned must not become owner regardless of FromDurableQueue")
	require.Zero(t, epoch2)
	require.Equal(t, MbOwned, mb.State, "state must remain owned")
	require.Empty(t, mb.Submitted,
		"a durable-Queue call must NOT be appended to mb.Submitted when the Mailbox is busy — "+
			"the durable row is its own retry path; queuing it here would double-execute it "+
			"once via the live owner's drain and once via the pump's re-lease after backoff")
}

// TestMailbox_Submit_NonDurableCallStillQueuesWhenAlreadyOwned is the
// companion proving the P0-1 fix does not change behavior for ordinary
// (non-durable) calls — the fast in-process handoff via mb.Submitted must
// still work for normal web/CLI turns and cross-process interrupt-Injects,
// which have no durable row backing them and rely on the Mailbox Queue as
// their ONLY retry path.
func TestMailbox_Submit_NonDurableCallStillQueuesWhenAlreadyOwned(t *testing.T) {
	mb := &Mailbox{}
	first := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "live owner's own turn"}
	ordinary := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "ordinary web turn", FromDurableQueue: false}

	becomeOwner1, _ := mb.Submit(first, func() {})
	require.True(t, becomeOwner1)

	becomeOwner2, epoch2 := mb.Submit(ordinary, func() {})
	require.False(t, becomeOwner2)
	require.Zero(t, epoch2)
	require.Len(t, mb.Submitted, 1, "an ordinary (non-durable) call must still be queued in mb.Submitted")
	require.Equal(t, ordinary, mb.Submitted[0])
}

// TestMailbox_Submit_QueuesWhenReleasing is the direct unit-level companion
// to the invariant-table/lock tests: Submit() must treat MbReleasing (#296/
// P1-C) exactly like MbOwned — Queue the call rather than become owner —
// since the OS session lock may still be held by the in-flight release()
// call even though no turn loop is currently running.
func TestMailbox_Submit_QueuesWhenReleasing(t *testing.T) {
	mb := &Mailbox{State: MbReleasing}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "during release"}

	becomeOwner, Epoch := mb.Submit(call, func() {})

	require.False(t, becomeOwner, "Submit during MbReleasing must NOT become owner — the OS lock may still be held")
	require.Zero(t, Epoch)
	require.Equal(t, MbReleasing, mb.State, "Submit must not alter mb.State while releasing")
	require.Len(t, mb.Submitted, 1)
	require.Equal(t, call, mb.Submitted[0])
}

// TestMailbox_InterruptAndReplace_NoOwnerWhenReleasing is the direct
// unit-level companion proving InterruptAndReplace() also treats MbReleasing
// as "nobody running" (gated on `state != MbOwned`): there is no live
// Generation left to interrupt once a turn has reached MbReleasing.
func TestMailbox_InterruptAndReplace_NoOwnerWhenReleasing(t *testing.T) {
	mb := &Mailbox{State: MbReleasing}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "interrupt during release"}

	cancel, hadOwner := mb.InterruptAndReplace(call)

	require.False(t, hadOwner, "InterruptAndReplace during MbReleasing must report no live owner to interrupt")
	require.Nil(t, cancel)
	require.Nil(t, mb.Replacement, "InterruptAndReplace must not record a Replacement when it reports no owner")
}

// TestMailbox_InterruptAndReplace_DurableCallSkipsReplacement is the direct
// unit-level regression test for P0-1 interrupt double-execution
// (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
// InterruptAndReplace must NOT set mb.Replacement when FromDurableQueue=true,
// otherwise the interrupt executes twice: once via mb.Replacement (live owner)
// and once via the pump (durable row).
//
// The durable Queue is now the sole owner of interrupt calls. InterruptAndReplace
// still cancels the in-flight Generation (if any) but skips recording mb.Replacement
// for durable calls, because the durable row itself is the execution path.
//
// REVERT CHECK: remove the `if !call.FromDurableQueue` guard in InterruptAndReplace
// (restore unconditional `mb.Replacement = &call`), this test fails (Replacement
// is set instead of remaining nil); restore the guard, it passes again.
func TestMailbox_InterruptAndReplace_DurableCallSkipsReplacement(t *testing.T) {
	mb := &Mailbox{
		State:   MbOwned,
		Epoch:   1,
		Current: Generation{ID: 1, Cancel: func() {}},
	}

	// Interrupt with a durable-Queue call (FromDurableQueue=true)
	durableCall := agentcall.SessionAgentCall{
		SessionID:        "s1",
		Prompt:           "interrupt from durable Queue",
		FromDurableQueue: true,
	}

	cancel, hadOwner := mb.InterruptAndReplace(durableCall)

	require.True(t, hadOwner, "InterruptAndReplace should report there was a live owner")
	require.NotNil(t, cancel, "InterruptAndReplace should return a cancel function for the in-flight Generation")

	// KEY ASSERTION: mb.Replacement must remain NIL for durable calls
	// If mb.Replacement were set, the live owner would execute it immediately
	// while the durable row remained pending for the pump to execute again.
	require.Nil(t, mb.Replacement,
		"InterruptAndReplace must NOT set mb.Replacement for FromDurableQueue=true calls "+
			"— the durable Queue is the sole owner; setting mb.Replacement would cause double-execution")
}

// TestMailbox_InterruptAndReplace_NonDurableCallStillSetsReplacement is the
// companion proving the fix does not change behavior for ordinary (non-durable)
// interrupts — the fast in-process handoff via mb.Replacement must still work
// for normal interrupts that don't have a durable row backing them.
func TestMailbox_InterruptAndReplace_NonDurableCallStillSetsReplacement(t *testing.T) {
	mb := &Mailbox{
		State:   MbOwned,
		Epoch:   1,
		Current: Generation{ID: 1, Cancel: func() {}},
	}

	// Interrupt with a non-durable call (FromDurableQueue=false)
	ordinaryCall := agentcall.SessionAgentCall{
		SessionID:        "s1",
		Prompt:           "ordinary interrupt",
		FromDurableQueue: false,
	}

	cancel, hadOwner := mb.InterruptAndReplace(ordinaryCall)

	require.True(t, hadOwner, "InterruptAndReplace should report there was a live owner")
	require.NotNil(t, cancel, "InterruptAndReplace should return a cancel function")

	// KEY ASSERTION: mb.Replacement SHOULD be set for non-durable calls
	// This is the normal interrupt path: mb.Replacement is the in-process handoff
	// from InterruptAndReplace to the next Generation.
	require.NotNil(t, mb.Replacement, "InterruptAndReplace must set mb.Replacement for non-durable calls")
	require.Equal(t, ordinaryCall, *mb.Replacement, "mb.Replacement should be the interrupt call")
}

func TestMailbox_DrainOrRelease_WithQueuedItem(t *testing.T) {
	mb := &Mailbox{
		State:     MbOwned,
		Epoch:     1,
		Current:   Generation{ID: 3, Cancel: func() {}},
		Submitted: []agentcall.SessionAgentCall{{SessionID: "s1", Prompt: "next"}},
	}

	next, ok := mb.DrainOrRelease(1)

	require.True(t, ok, "a queued call must be returned")
	require.Equal(t, "next", next.Prompt)
	require.Empty(t, mb.Submitted, "the drained item must be removed from the Queue")
	require.Equal(t, MbOwned, mb.State, "state must stay owned when there is more work")
	require.NotZero(t, mb.Current.ID, "current Generation must be left untouched when staying owned")
}

func TestMailbox_DrainOrRelease_EmptyFlipsToIdle(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 7, Cancel: func() {}},
	}

	next, ok := mb.DrainOrRelease(1)

	require.False(t, ok, "no queued call must be reported")
	require.Equal(t, agentcall.SessionAgentCall{}, next)
	require.Equal(t, MbIdle, mb.State, "state must flip to idle only when nothing was queued")
	require.Equal(t, uint64(7), mb.Current.ID,
		"Generation id must be preserved across a release (round 9 review MEDIUM-2) — only the spent cancel func is cleared, "+
			"so a future Inject stamped against this id is not wrongly treated as belonging to a not-yet-started future Generation")
	require.Nil(t, mb.Current.Cancel, "the spent cancel func must be cleared on release")
	require.Nil(t, mb.DispatcherCancel, "dispatcherCancel must be cleared on release")
}

// TestMailbox_DrainOrRelease_EpochMismatch_IsNoOp is the regression test for
// release-blocker BLOCKER-2 (round 9 review): a stale DrainOrRelease call —
// e.g. Run's cleanup defer firing after a DIFFERENT, later owner has already
// claimed the Mailbox — must not touch that later owner's state at all.
func TestMailbox_DrainOrRelease_EpochMismatch_IsNoOp(t *testing.T) {
	laterOwnerCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "later owner's call"}
	mb := &Mailbox{
		State:     MbOwned,
		Epoch:     2, // a NEW era, e.g. claimed by a concurrent Submit after the stale caller's era ended
		Current:   Generation{ID: 9, Cancel: func() {}},
		Submitted: []agentcall.SessionAgentCall{laterOwnerCall},
	}

	next, ok := mb.DrainOrRelease(1) // stale caller presents an OLD Epoch

	require.False(t, ok, "a stale release must report nothing drained, not silently steal the later owner's queued call")
	require.Equal(t, agentcall.SessionAgentCall{}, next)
	require.Equal(t, MbOwned, mb.State, "a stale release must not touch a different owner's state")
	require.Equal(t, uint64(2), mb.Epoch, "Epoch must be untouched by a stale release")
	require.Len(t, mb.Submitted, 1, "the later owner's queued call must survive a stale release untouched")
	require.Equal(t, laterOwnerCall, mb.Submitted[0])
}

func TestMailbox_InterruptAndReplace_NoOwnerReturnsFalse(t *testing.T) {
	mb := &Mailbox{}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "replace-me"}

	cancel, hadOwner := mb.InterruptAndReplace(call)

	require.False(t, hadOwner, "InterruptAndReplace on an idle Mailbox must report no owner")
	require.Nil(t, cancel)
	require.Nil(t, mb.Replacement, "no Replacement should be recorded when there is no owner")
}

func TestMailbox_InterruptAndReplace_OwnedRecordsReplacementAndReturnsCurrentCancel(t *testing.T) {
	currentCancelCalled := false
	currentCancel := func() { currentCancelCalled = true }
	mb := &Mailbox{
		State:   MbOwned,
		Current: Generation{ID: 5, Cancel: currentCancel},
	}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "replace-me"}

	cancel, hadOwner := mb.InterruptAndReplace(call)

	require.True(t, hadOwner, "InterruptAndReplace on an owned Mailbox must report an owner existed")
	require.NotNil(t, mb.Replacement, "the Replacement must be recorded")
	require.Equal(t, call, *mb.Replacement)
	require.NotNil(t, cancel, "the current Generation's cancel func must be returned")

	// The returned cancel must be the CURRENT Generation's cancel, not
	// something else — verify by invoking it and observing the same
	// side effect the original currentCancel would produce.
	cancel()
	require.True(t, currentCancelCalled, "returned cancel must be the current Generation's cancel func")
}

// TestMailbox_InterruptAndReplace_OwnedButNoLiveGeneration_RecordsReplacementWithoutDispatcherFallback
// is the direct unit-level regression test for #307 (P1-2 follow-up): the
// inter-turn window, where mb.State == MbOwned (the era is still open, a
// turn loop iteration is about to run) but mb.Current.Cancel == nil (no
// Generation is actually live — see DrainOrRelease/DrainOrReleaseFinal/
// DrainAfterCancel's every "keep ownership" branch, all of which clear
// current.cancel as part of the SAME postcondition
// mailbox_invariant_test.go's table checks).
//
// Before the fix, InterruptAndReplace fell back to mb.DispatcherCancel in
// this exact situation (round 9 review, MEDIUM-1's fallback, originally
// added to cover the narrow pre-first-Generation window and never
// reconsidered for the inter-turn window opened up by #284's turnCtx/
// turnCancel split). dispatcherCancel is runCancel — the parent context of
// EVERY future turn, never meant to be an interrupt target (see the field's
// own doc) — so firing it here killed the whole dispatcher instead of just
// failing to cancel a nonexistent Generation.
func TestMailbox_InterruptAndReplace_OwnedButNoLiveGeneration_RecordsReplacementWithoutDispatcherFallback(t *testing.T) {
	dispatcherCancelCalled := false
	mb := &Mailbox{
		State:            MbOwned,
		DispatcherCancel: func() { dispatcherCancelCalled = true },
		Current:          Generation{ID: 3, Cancel: nil}, // the inter-turn window: owned, but no live Generation
	}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "replace-me"}

	cancel, hadOwner := mb.InterruptAndReplace(call)

	require.True(t, hadOwner, "an owned Mailbox must still report an owner existed, even with no live Generation — "+
		"the caller's contract is 'accepted, will run next', not 'a Generation was actually interrupted'")
	require.NotNil(t, mb.Replacement, "the Replacement must still be recorded")
	require.Equal(t, call, *mb.Replacement)
	require.Nil(t, cancel, "must NOT fall back to dispatcherCancel — firing it would cancel runCtx, the parent of "+
		"every future turn's context, not just the (nonexistent) current Generation")

	require.False(t, dispatcherCancelCalled, "dispatcherCancel must never be invoked by InterruptAndReplace — "+
		"only HardStop/CancelAll are allowed to touch it")
}

// TestMailbox_ReclaimReplacementOrKeep_* covers the loop-side half of the
// #307 fix: ReclaimReplacementOrKeep is called by Run's turn loop
// (agent.go) at the TestLoopRearmSeam point, immediately before
// BeginGeneration, specifically so a Replacement recorded during the
// inter-turn window pre-empts the stale `call` the previous turn's own
// drain already decided on, instead of that stale call running to
// completion first.
func TestMailbox_ReclaimReplacementOrKeep_NoReplacementReturnsCallUnchanged(t *testing.T) {
	mb := &Mailbox{}
	call := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "the queued call"}

	got := mb.ReclaimReplacementOrKeep(call)

	require.Equal(t, call, got, "with no Replacement recorded, the loop's own call must be returned unchanged")
	require.Nil(t, mb.Replacement)
}

func TestMailbox_ReclaimReplacementOrKeep_ReplacementPreemptsStaleCall(t *testing.T) {
	staleCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "stale queued call"}
	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement from InterruptAndReplace"}
	mb := &Mailbox{
		Replacement: &Replacement,
	}

	got := mb.ReclaimReplacementOrKeep(staleCall)

	require.Equal(t, Replacement.SessionID, got.SessionID, "a recorded Replacement must pre-empt the loop's stale call, not merely "+
		"run after it — this is what makes InterruptAndReplace actually mean replace instead of Queue-behind")
	require.Equal(t, Replacement.Prompt, got.Prompt)
	require.True(t, got.ReplacementHandoff)
	require.Nil(t, mb.Replacement, "the Replacement must be cleared once consumed, so it is not run a second time "+
		"by a later drain")
	require.Equal(t, []agentcall.SessionAgentCall{staleCall}, mb.Submitted, "the pre-empted stale call must NOT be discarded — "+
		"it must be pushed back onto mb.Submitted so a later drain still runs it (closing review after the first "+
		"draft of this fix: destroying it here would be the exact #283/P0-2 class of bug, 'interrupt deletes the "+
		"very message it's supposed to Queue behind')")
}

// TestMailbox_ReclaimReplacementOrKeep_MultiElementQueue_OnlyReordersNothingIsLost is
// the direct unit-level regression test for the closing-review defect: with
// MORE than one message already queued, a prior draft of this method
// silently destroyed exactly the one message a drain had already popped out
// of mb.Submitted into `call`, while any siblings still sitting in
// mb.Submitted survived untouched — the choice of victim decided purely by
// scheduling luck (whether the interrupt landed a moment before or after
// the drain popped `call`). This test simulates that exact scenario: A was
// already popped out of mb.Submitted (so it arrives here as `call`), B and
// C are still queued, and D lands as the Replacement.
func TestMailbox_ReclaimReplacementOrKeep_MultiElementQueue_OnlyReordersNothingIsLost(t *testing.T) {
	callA := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "A - already popped by a prior drain, arrives as `call`"}
	callB := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "B - still queued"}
	callC := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "C - still queued"}
	callD := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "D - the interrupt's Replacement"}
	mb := &Mailbox{
		Replacement: &callD,
		Submitted:   []agentcall.SessionAgentCall{callB, callC},
	}

	got := mb.ReclaimReplacementOrKeep(callA)

	require.Equal(t, callD.SessionID, got.SessionID, "D must run next — the interrupt's whole point is pre-empting whatever the loop "+
		"was about to run")
	require.Equal(t, callD.Prompt, got.Prompt)
	require.True(t, got.ReplacementHandoff)
	require.Nil(t, mb.Replacement)
	require.Equal(t, []agentcall.SessionAgentCall{callA, callB, callC}, mb.Submitted,
		"A must be restored to the FRONT of mb.Submitted (ahead of B and C, preserving original FIFO order "+
			"among the deferred messages) — none of A, B, or C may be lost; only D is allowed to jump the Queue, "+
			"because the caller explicitly asked to interrupt-and-replace")
}

func TestMailbox_DrainAfterCancel_PrefersReplacementOverSubmitted(t *testing.T) {
	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued"}
	mb := &Mailbox{
		Replacement: &Replacement,
		Submitted:   []agentcall.SessionAgentCall{queued},
	}

	next, ok := mb.DrainAfterCancel()

	require.True(t, ok)
	require.Equal(t, Replacement.SessionID, next.SessionID, "Replacement must win over submitted")
	require.Equal(t, Replacement.Prompt, next.Prompt)
	require.True(t, next.ReplacementHandoff)
	require.Nil(t, mb.Replacement, "Replacement must be cleared after being drained")
	require.Len(t, mb.Submitted, 1, "submitted Queue must be untouched when Replacement was drained")
}

func TestMailbox_DrainAfterCancel_FallsBackToSubmittedWhenNoReplacement(t *testing.T) {
	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued"}
	mb := &Mailbox{
		Submitted: []agentcall.SessionAgentCall{queued},
	}

	next, ok := mb.DrainAfterCancel()

	require.True(t, ok)
	require.Equal(t, queued, next)
	require.Empty(t, mb.Submitted, "the drained item must be removed from the Queue")
}

func TestMailbox_DrainAfterCancel_NeitherReturnsFalse(t *testing.T) {
	mb := &Mailbox{}

	next, ok := mb.DrainAfterCancel()

	require.False(t, ok)
	require.Equal(t, agentcall.SessionAgentCall{}, next)
}

func TestMailbox_Inject_StampsCurrentGenerationID(t *testing.T) {
	mb := &Mailbox{Current: Generation{ID: 4}}
	msg := message.Message{ID: "m1"}

	mb.Inject(msg)

	require.Len(t, mb.Injects, 1)
	require.Equal(t, msg, mb.Injects[0].Msg)
	require.Equal(t, uint64(4), mb.Injects[0].AfterGenID)
}

func TestMailbox_Inject_ZeroGenerationIsValidStamp(t *testing.T) {
	mb := &Mailbox{} // current.id defaults to 0 (no owner yet)
	msg := message.Message{ID: "m0"}

	mb.Inject(msg)

	require.Len(t, mb.Injects, 1)
	require.Equal(t, uint64(0), mb.Injects[0].AfterGenID, "0 must be accepted as a meaningful Generation stamp")
}

func TestMailbox_DrainInjects_SplitsDueFromFuture(t *testing.T) {
	mb := &Mailbox{
		Injects: []PendingInject{
			{Msg: message.Message{ID: "past"}, AfterGenID: 1},
			{Msg: message.Message{ID: "current"}, AfterGenID: 2},
			{Msg: message.Message{ID: "future"}, AfterGenID: 3},
		},
	}

	due := mb.DrainInjects(2)

	require.Len(t, due, 2, "entries stamped <= genID must be due")
	gotIDs := []string{due[0].Msg.ID, due[1].Msg.ID}
	require.ElementsMatch(t, []string{"past", "current"}, gotIDs)

	require.Len(t, mb.Injects, 1, "future-stamped entries must remain queued")
	require.Equal(t, "future", mb.Injects[0].Msg.ID)
}

func TestMailbox_DrainInjects_AllDueEmptiesQueue(t *testing.T) {
	mb := &Mailbox{
		Injects: []PendingInject{
			{Msg: message.Message{ID: "a"}, AfterGenID: 1},
			{Msg: message.Message{ID: "b"}, AfterGenID: 1},
		},
	}

	due := mb.DrainInjects(5)

	require.Len(t, due, 2)
	require.Empty(t, mb.Injects)
}

func TestMailbox_DrainInjects_NoneDueLeavesQueueIntact(t *testing.T) {
	mb := &Mailbox{
		Injects: []PendingInject{
			{Msg: message.Message{ID: "future"}, AfterGenID: 10},
		},
	}

	due := mb.DrainInjects(2)

	require.Empty(t, due)
	require.Len(t, mb.Injects, 1)
}

func TestMailbox_BeginGeneration_IncrementsID(t *testing.T) {
	mb := &Mailbox{}

	id1 := mb.BeginGeneration(func() {})
	require.Equal(t, uint64(1), id1)

	id2 := mb.BeginGeneration(func() {})
	require.Equal(t, uint64(2), id2)

	require.NotEqual(t, id1, id2, "every call must produce a unique id")
}

func TestMailbox_BeginGeneration_RecordsCancelAsCurrent(t *testing.T) {
	mb := &Mailbox{}
	called := false
	cancel := func() { called = true }

	genID := mb.BeginGeneration(cancel)

	require.Equal(t, genID, mb.Current.ID)
	mb.Current.Cancel()
	require.True(t, called, "BeginGeneration must record the passed cancel as current.cancel")
}

// A light end-to-end style test tying Submit -> InterruptAndReplace ->
// DrainAfterCancel -> BeginGeneration together, mirroring the flow
// described in design §4 (steps 1-4), still entirely within the Mailbox's
// own API surface (no sessionAgent involved).
func TestMailbox_InterruptThenDrainAfterCancel_SequenceRoundTrips(t *testing.T) {
	mb := &Mailbox{}

	first := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "first"}
	becomeOwner, _ := mb.Submit(first, func() {})
	require.True(t, becomeOwner)

	genCtx, genCancel := context.WithCancel(context.Background())
	genID := mb.BeginGeneration(genCancel)
	require.Equal(t, uint64(1), genID)

	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	cancelFn, hadOwner := mb.InterruptAndReplace(Replacement)
	require.True(t, hadOwner)
	require.NotNil(t, cancelFn)

	cancelFn()
	require.Error(t, genCtx.Err())

	next, ok := mb.DrainAfterCancel()
	require.True(t, ok)
	require.Equal(t, Replacement.SessionID, next.SessionID)
	require.Equal(t, Replacement.Prompt, next.Prompt)
	require.True(t, next.ReplacementHandoff)
}

// TestMailbox_DrainOrRelease_ConcurrentSubmitInFinalDrainWindow_P0_3 is the
// mandatory deterministic regression test for release-blocker P0-3
// (docs/reviews/2026-08-04-multi-agent-stability-follow-up.md, release-gate
// scenario 3): "a plain concurrent send that lands exactly in the
// final-drain-to-release window must either become the new owner or be
// guaranteed to run under the old owner — never orphaned."
//
// Before this migration, the owner's final drain (messageQueue.PopFront
// returning empty) and the reservation release (activeRequests.Del, run via
// Run()'s deferred releaseSessionReservation) were two SEPARATE, non-atomic
// steps. A concurrent Submit landing in the gap between them would observe
// "still busy" (the map entry not yet deleted), append itself to
// messageQueue, and then nobody would ever look at that Queue entry again —
// the owner had already decided "nothing queued" and moved on, and the
// concurrent caller had already returned believing its message was queued
// for the (already-departing) owner to pick up.
//
// This test reproduces that EXACT window deterministically — not
// probabilistically — using Mailbox.TestDrainSeam: a hook DrainOrRelease
// invokes strictly after observing mb.Submitted empty but strictly before
// flipping mb.State to MbIdle (i.e., precisely inside what is now one
// critical section but used to be the gap between two). Because
// mb.Mu is held for the whole of DrainOrRelease (including the hook call),
// a concurrent Submit() blocks on mb.Mu.Lock() until the hook returns and
// DrainOrRelease's own critical section finishes — so the test does not
// need to win a race against goroutine scheduling to land the Submit inside
// the window; the window is held open by the mutex until the test releases
// it.
func TestMailbox_DrainOrRelease_ConcurrentSubmitInFinalDrainWindow_P0_3(t *testing.T) {
	const ownerEpoch = 1
	mb := &Mailbox{
		State:   MbOwned,
		Epoch:   ownerEpoch,
		Current: Generation{ID: 1, Cancel: func() {}},
	}

	seamEntered := make(chan struct{})
	releaseSeam := make(chan struct{})
	mb.TestDrainSeam = func() {
		close(seamEntered)
		<-releaseSeam
	}

	var (
		wg                    sync.WaitGroup
		concurrentBecameOwner bool
		concurrentEpoch       uint64
	)
	concurrentCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "concurrent-during-final-drain"}

	// Owner's final drain, running in its own goroutine so the test can
	// observe it paused mid-critical-section via seamEntered.
	drainResult := make(chan struct {
		next agentcall.SessionAgentCall
		ok   bool
	}, 1)
	wg.Go(func() {
		next, ok := mb.DrainOrRelease(ownerEpoch)
		drainResult <- struct {
			next agentcall.SessionAgentCall
			ok   bool
		}{next, ok}
	})

	// Wait until DrainOrRelease has entered the hook — i.e. has already
	// observed mb.Submitted empty and is about to flip state to MbIdle, but
	// has NOT released mb.Mu yet (the hook itself runs under mb.Mu, and
	// mb.Mu is not released until DrainOrRelease's deferred Unlock runs
	// after the hook returns).
	select {
	case <-seamEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainOrRelease never reached the test seam")
	}

	// Fire the concurrent Submit NOW, while DrainOrRelease is paused inside
	// its own critical section holding mb.Mu. Submit() must block on
	// mb.Mu.Lock() until we release the seam below — proving the two
	// critical sections cannot interleave.
	submitReturned := make(chan struct{})
	wg.Go(func() {
		concurrentBecameOwner, concurrentEpoch = mb.Submit(concurrentCall, func() {})
		close(submitReturned)
	})

	// Submit() must NOT be able to proceed while the seam is held — give it
	// a moment on a real scheduler and confirm it is still blocked. This
	// turns "the two critical sections are mutually exclusive" from an
	// assumption into an observed fact for this run.
	select {
	case <-submitReturned:
		t.Fatal("Submit() returned before DrainOrRelease released the Mailbox lock — critical sections overlapped")
	case <-time.After(50 * time.Millisecond):
	}

	// Now let DrainOrRelease finish its critical section (flip to idle,
	// unlock). Exactly one of two outcomes is correct from here:
	//  1. DrainOrRelease's own critical section commits FIRST (state ->
	//     MbIdle, lock released) and Submit() then acquires the lock and
	//     sees MbIdle -> becomes the new owner (concurrentBecameOwner ==
	//     true). This is what will actually happen here, since Submit() is
	//     already blocked waiting on mb.Mu when we release the seam.
	// Either way, the message is never orphaned: it either becomes owned by
	// the concurrent caller (fresh Run()) or would have been picked up by
	// the departing owner had it arrived a moment earlier (covered by the
	// sibling "WithQueuedItem" test above). What must NEVER happen is what
	// the old code allowed: the departing owner decides "nothing queued"
	// AND the concurrent caller decides "still busy, I'll just Queue" —
	// both true at once, with nobody left to drain the Queue.
	close(releaseSeam)
	wg.Wait()

	res := <-drainResult
	require.False(t, res.ok, "the owner's own drain found nothing queued at the instant it checked")
	require.True(t, concurrentBecameOwner,
		"the concurrent Submit landing in the old lost-wakeup window must become the new owner instead of being silently queued with nobody left to drain it")
	require.Equal(t, MbOwned, mb.State, "the concurrent Submit becoming owner must leave the Mailbox owned, not idle")
	require.Empty(t, mb.Submitted, "the concurrent call must have been picked up as the new owner's call, not left sitting in submitted with no reader")

	// Round 9 review, BLOCKER-2: the concurrent owner's era must be a NEW
	// Epoch, distinct from the departing owner's. This is what makes a
	// stale DrainOrRelease call from the departing owner's own cleanup
	// path (Run's defer, presenting ownerEpoch) a safe no-op afterward
	// instead of being able to clobber this new owner's state — see
	// TestMailbox_DrainOrRelease_EpochMismatch_IsNoOp for that half.
	require.NotEqual(t, uint64(ownerEpoch), concurrentEpoch, "the new owner must be granted a NEW ownership era, not reuse the departing owner's")
	require.Equal(t, concurrentEpoch, mb.Epoch, "the Mailbox's current Epoch must match what the new owner was granted")
}

// TestMailbox_DrainOrRelease_NoSeamHook_BehavesIdentically is a sanity check
// that a nil TestDrainSeam (the case for every real, non-test Mailbox) makes
// DrainOrRelease behave exactly as it did before the hook was added — the
// hook must be a strict no-op addition, never a behavior change.
func TestMailbox_DrainOrRelease_NoSeamHook_BehavesIdentically(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 9, Cancel: func() {}},
	}

	next, ok := mb.DrainOrRelease(1)

	require.False(t, ok)
	require.Equal(t, agentcall.SessionAgentCall{}, next)
	require.Equal(t, MbIdle, mb.State)
	require.Equal(t, uint64(9), mb.Current.ID, "Generation id is preserved across release (round 9 review MEDIUM-2)")
	require.Nil(t, mb.Current.Cancel)
	require.Nil(t, mb.DispatcherCancel)
}

// TestMailbox_AbandonOwnership_EmptyEndsAtIdle is the round 9 review's
// BLOCKER-2a regression test: Run's cleanup defer must not leave the
// Mailbox permanently MbOwned with nobody running it. Even with nothing
// queued, AbandonOwnership must end the era at idle.
func TestMailbox_AbandonOwnership_EmptyEndsAtIdle(t *testing.T) {
	mb := &Mailbox{
		State:            MbOwned,
		Epoch:            1,
		DispatcherCancel: func() {},
		Current:          Generation{ID: 3, Cancel: func() {}},
	}

	hadWork := mb.AbandonOwnership(1)

	require.False(t, hadWork, "nothing was queued")
	require.Equal(t, MbIdle, mb.State)
	require.Nil(t, mb.DispatcherCancel)
	require.Nil(t, mb.Current.Cancel)
}

// TestMailbox_AbandonOwnership_WithQueuedWork_LeavesEntriesAndEndsAtIdle is
// the core BLOCKER-2a regression: before this method existed, Run's cleanup
// defer called DrainOrRelease/drainOrReleaseMerged, whose "found something"
// branch leaves state == MbOwned expecting the CALLER to run it as the next
// turn. Run's defer has no turn loop left to do that, so the old code silently
// wedged the session permanently busy. AbandonOwnership must ALWAYS end at
// idle, whether or not anything was queued. Since #308, it leaves entries in
// submitted (folding any pending Replacement in) for the next owner to drain
// — the Mailbox's submitted Queue is now the single source of truth.
func TestMailbox_AbandonOwnership_WithQueuedWork_LeavesEntriesAndEndsAtIdle(t *testing.T) {
	queued := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "queued during the failed turn"}
	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "an interrupt-and-replace nobody got to run"}
	mb := &Mailbox{
		State:       MbOwned,
		Epoch:       1,
		Current:     Generation{ID: 5, Cancel: func() {}},
		Submitted:   []agentcall.SessionAgentCall{queued},
		Replacement: &Replacement,
	}

	hadWork := mb.AbandonOwnership(1)

	require.True(t, hadWork)
	// The entries remain in mb.Submitted (Replacement folded in) for the
	// next owner to drain.
	require.Len(t, mb.Submitted, 2, "both submitted and Replacement entries should remain in mb.Submitted")
	require.ElementsMatch(t, []agentcall.SessionAgentCall{queued, Replacement}, mb.Submitted)
	require.Equal(t, MbIdle, mb.State,
		"the Mailbox must end up idle even though something was queued — there is no turn loop left to keep it owned for (BLOCKER-2a)")
	require.Nil(t, mb.Replacement, "Replacement must be folded into submitted, not left as a separate field")

	// A NEW Run() for this session must be able to claim it fresh.
	becomeOwner, newEpoch := mb.Submit(agentcall.SessionAgentCall{SessionID: "s1", Prompt: "fresh start"}, func() {})
	require.True(t, becomeOwner, "the session must not be permanently wedged busy — a later Run() must be able to claim it")
	require.NotEqual(t, uint64(1), newEpoch)
}

// TestMailbox_AbandonOwnership_EpochMismatch_IsNoOp mirrors
// TestMailbox_DrainOrRelease_EpochMismatch_IsNoOp for AbandonOwnership: a
// stale call (the era already ended, e.g. runTurn's own drain already ran,
// or a concurrent Submit became a new owner) must not touch a different,
// current owner's state.
func TestMailbox_AbandonOwnership_EpochMismatch_IsNoOp(t *testing.T) {
	laterOwnerCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "later owner's call"}
	mb := &Mailbox{
		State:     MbOwned,
		Epoch:     2,
		Current:   Generation{ID: 9, Cancel: func() {}},
		Submitted: []agentcall.SessionAgentCall{laterOwnerCall},
	}

	hadWork := mb.AbandonOwnership(1) // stale Epoch

	require.False(t, hadWork)
	require.Equal(t, MbOwned, mb.State, "a stale abandon must not touch a different owner's state")
	require.Equal(t, uint64(2), mb.Epoch)
	require.Len(t, mb.Submitted, 1)
	require.Equal(t, laterOwnerCall, mb.Submitted[0], "the later owner's queued call must survive a stale abandon call untouched")
}

// TestMailbox_ReclaimReplacementOrKeep_DisplacedDrainDroppedWhenQueueNonEmpty
// pins B17 (docs/reviews/2026-09-29-async-phase4-round1.md): a displaced
// Drain being pushed back onto the Queue must go through the SAME Drain-
// merge rule as every other insertion path (doc sec.3.4: "at most one Drain
// call in the Queue, on ANY insertion path, including returning a displaced
// call to the head") -- a bare prepend let it sit ALONGSIDE whatever was
// already queued.
//
// Revert-check performed: changed ReclaimReplacementOrKeep back to `mb.
// submitted = append([]agentcall.SessionAgentCall{call}, mb.Submitted...)` (bare
// prepend) -- this test's `require.Equal(t, []agentcall.SessionAgentCall{callB},
// mb.Submitted)` FAILED (mb.Submitted was [drainCall, callB]: two entries,
// the displaced Drain sitting in front of the already-queued real call).
// Restored the mergeQueuedCallAtHead call; re-ran, passed.
func TestMailbox_ReclaimReplacementOrKeep_DisplacedDrainDroppedWhenQueueNonEmpty(t *testing.T) {
	drainCall := testDrainCall(agentcall.SessionAgentCall{SessionID: "s1"})
	callB := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "B - already queued"}
	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	mb := &Mailbox{
		Replacement: &Replacement,
		Submitted:   []agentcall.SessionAgentCall{callB},
	}

	got := mb.ReclaimReplacementOrKeep(drainCall)

	require.Equal(t, Replacement.Prompt, got.Prompt)
	require.Equal(t, []agentcall.SessionAgentCall{callB}, mb.Submitted,
		"a displaced Drain must be dropped, not queued alongside an already-non-empty Queue")
}

// TestMailbox_ReclaimReplacementOrKeep_DisplacedRealCallDropsQueuedDrain is
// the mirror: a displaced REAL call reclaiming the head must drop an
// already-queued Drain (the stronger policy wins, matching mergeQueuedCall's
// own rule for the tail-insertion path).
func TestMailbox_ReclaimReplacementOrKeep_DisplacedRealCallDropsQueuedDrain(t *testing.T) {
	displacedReal := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "displaced real call"}
	queuedDrain := testDrainCall(agentcall.SessionAgentCall{SessionID: "s1"})
	Replacement := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	mb := &Mailbox{
		Replacement: &Replacement,
		Submitted:   []agentcall.SessionAgentCall{queuedDrain},
	}

	got := mb.ReclaimReplacementOrKeep(displacedReal)

	require.Equal(t, Replacement.Prompt, got.Prompt)
	require.Equal(t, []agentcall.SessionAgentCall{displacedReal}, mb.Submitted,
		"a displaced real call must drop an already-queued Drain, not sit behind or alongside it")
}

func testDrainCall(base agentcall.SessionAgentCall) agentcall.SessionAgentCall {
	base.IsDrain = true
	base.AutoResumed = true
	base.BackgroundJobNotice = true
	return base
}
