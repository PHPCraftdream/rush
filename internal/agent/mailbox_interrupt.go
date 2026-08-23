// Mailbox interrupt and cancellation paths: hardStop/isStopped,
// interruptAndReplace with its inter-turn reclaimReplacementOrKeep, and
// the generation-aware drainAfterCancel. Pure code move from mailbox.go.

package agent

import (
	"context"
)

// hardStop implements the shutdown half of P0-C: it latches the mailbox
// closed and returns the dispatcher cancel (the DURABLE, whole-Run()
// CancelFunc) plus the current generation's cancel, for the caller to
// invoke outside mb.mu.
//
// Unlike Cancel/interruptAndReplace — which deliberately target only the
// current generation so a turn loop survives to run the replacement —
// this is the operation that must actually END the dispatcher: cancelling
// dispatcherCancel unwinds Run() itself, and `stopped` guarantees that on
// the way out no drain hands it another call to run.
//
// Anything still queued is left in place rather than discarded, and since
// task #340 leaving it in place IS a save: when the latch refuses the
// turn-loop drains and Run() unwinds, Run's deferred
// abandonOwnershipWithHandoff pops the queue and durably enqueues every
// queued call via restartOrphanedWithRetry — a session_run_queue row, not
// a provider turn. (An earlier version of this comment claimed the queue
// was "genuinely lost" because a queued SessionAgentCall is not a DB row;
// that was true pre-#340, when restart meant a detached Run(), but is
// obsolete now.) The only thing the latch itself must prevent is another
// in-process turn: the shutdown admission gate (tryAdmitRunWg) refuses
// any Run during shutdown, and App.Shutdown stops the pump before
// closing the DB, so the durable rows are safely processed later — after
// restart, in a fresh process. This includes interrupt tick's calls (an
// ExistingMessageID for a row `crush sessions inject` already wrote):
// they ride the same durable-enqueue path as everything else, they just
// additionally already had a DB row.
//
// hardStop landing while state == mbReleasing (#296/P1-C: drainOrReleaseFinal
// is between dropping mb.mu to run release() and reacquiring it to finalize)
// needs no special case here: it takes mb.mu for the single instant needed
// to set the latch and read the two cancel funcs, so it is never blocked by
// the in-flight, lock-free release() call — that is the entire reason that
// call no longer holds mb.mu. Both funcs read back nil (cleared on entry to
// mbReleasing — see that transition), so the caller's cancel invocations are
// correctly no-ops: there is no live generation left to interrupt, the turn
// loop is already past its provider work and mid lock-teardown. The latch
// itself is still fully effective — but not because anything re-reads it
// at finalize: the finalize step is latch-blind (in effect since #646;
// #652 deleted the dead stopped branch outright) and ALWAYS drains work
// that raced in during the release() window out as orphaned for
// restartOrphaned's durable enqueue — not lost, and not handed back to
// the (now shutting-down) turn loop — whether or not a hardStop landed.
// The stopped-specific half lives in drainOrReleaseFinal's ENTRY check,
// which skips the live branches and keeps hasNext false when hardStop
// lands BEFORE the drain is even called (see its own doc in
// mailbox_ownership.go).
func (mb *mailbox) hardStop() (dispatcherCancel, genCancel context.CancelFunc) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.stopped = true
	return mb.dispatcherCancel, mb.current.cancel
}

// isStopped reports whether hardStop has latched this mailbox closed.
func (mb *mailbox) isStopped() bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.stopped
}

// interruptAndReplace implements design §4: the coordinator's single entry
// point for "interrupt and replace", replacing QueueMessage+Cancel as one
// atomic mailbox operation. When nobody owns the session, it returns
// hadOwner=false and does nothing further (the caller should instead start
// a fresh Run() with call directly). When a turn is in progress, it records
// call as the replacement to be consumed by the NEXT generation, and returns
// a cancel func the caller can invoke to interrupt the in-flight generation
// (outside the mailbox lock).
func (mb *mailbox) interruptAndReplace(call SessionAgentCall) (context.CancelFunc, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state != mbOwned || mb.stopped {
		// Nobody running: behave like a plain submit that also happens to
		// return "no cancel needed" — the caller (coordinator) then starts
		// a fresh Run() with call directly instead of relying on drain.
		//
		// `stopped` counts as "nobody running" too (closing review, blocker
		// 2), even while state is still mbOwned. Recording a replacement on
		// a hard-stopped mailbox would report success to the caller — and
		// through it "queued" to the web client — for a payload both drains
		// are now guaranteed to refuse, leaving it to be swept into an
		// in-memory queue by a process that is exiting. Returning false
		// instead routes the caller to its own no-owner path, where Run
		// refuses with ErrAgentShuttingDown so the failure is visible
		// rather than silent.
		//
		// mb.state == mbReleasing (#296/P1-C) also takes this branch, since
		// it fails `mb.state != mbOwned` too: there is no live generation
		// left to interrupt (the turn loop is already past its provider
		// work, mid lock-teardown), and current.cancel/dispatcherCancel are
		// both nil by the time a turn reaches mbReleasing anyway (cleared on
		// the way in — see drainOrReleaseFinal). The caller's fallback
		// ("start a fresh Run()") is correct here too: drainOrReleaseFinal's
		// finalize step is what actually decides whether that fresh call
		// gets handed back to THIS turn loop or a brand new Run() truly
		// starts, once release() returns.
		return nil, false
	}
	// Record the replacement FIRST, under the same lock that is about to cancel
	// the current generation. There is no window between "replacement is recorded"
	// and "current generation is cancelled" for an external observer to land in,
	// because both happen before mu is released.
	//
	// Owner: The replacement is owned by the current session's mailbox in-memory.
	// Durable record: For calls originating from durable queue (FromDurableQueue=true),
	// the durable row itself remains the authoritative record; the pump will execute it.
	// For non-durable calls, the mailbox replacement is the only retry path.
	// Cancellation authority: The returned cancel func interrupts the in-flight generation.
	// Terminal transition: The replacement becomes the next generation's call after cancellation.
	//
	// P0-1 fix (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
	// calls FROM the durable queue (call.FromDurableQueue) do NOT set
	// mb.replacement to avoid double-execution. The durable row itself is
	// already the retry path, and the pump will execute it. Setting mb.replacement
	// would create a live copy that runs immediately, leaving the durable row
	// pending for a second execution by the pump. For non-durable calls, the
	// mailbox replacement IS the only retry path, so they are recorded normally.
	//
	// "the pump will execute it" originally meant only the background
	// RunQueuePump's own periodic tick (3s in production) — leaving a real
	// liveness gap for short-lived processes (crush run), since the process
	// routinely exits before that tick ever fires. Task #421/P0-1 closed
	// that gap WITHOUT touching this guard: RunNonInteractive
	// (internal/app/app.go) now calls RunQueuePump.DrainSessionNow
	// synchronously in the same process once the cancelled generation
	// returns, executing the row through the exact same lease/execute path
	// the background tick uses. This function's guard above is still
	// correct and unchanged — it only decides mailbox ownership, not who
	// eventually runs the durable row.
	if !call.FromDurableQueue {
		mb.replacement = &call
	}
	// #307 (P1-2 follow-up): mb.current.cancel == nil while mb.state ==
	// mbOwned is NOT a special case restricted to "before the very first
	// generation" — it is the inter-turn window too: Run's loop clears
	// current.cancel (via the just-finished turn's own drain — every hit
	// branch of drainOrRelease/drainOrReleaseFinal/drainAfterCancel leaves
	// it nil, see mailbox_invariant_test.go) and does not repopulate it
	// until the NEXT iteration's beginGeneration call. In BOTH windows
	// (pre-first-generation and inter-turn) there is genuinely nothing
	// live to interrupt — the turn loop itself is between provider calls,
	// not inside one.
	//
	// A previous version of this method (round 9 review, MEDIUM-1) fell
	// back to mb.dispatcherCancel here on the theory that SOME cancel must
	// be returned or the replacement would be silently stranded. That was
	// wrong: dispatcherCancel is runCancel, the whole-Run() context every
	// future turn's genCtx descends from (see the field's own doc — "never
	// the target of an interrupt"). Firing it here doesn't just end the
	// current (nonexistent) generation, it poisons runCtx for every turn
	// the loop will ever create again, including the replacement's own —
	// the replacement gets recorded, then the loop's next iteration derives
	// a turnCtx from an already-cancelled runCtx, whose preamble immediately
	// fails with context.Canceled. That failure DOES recover the mailbox's
	// replacement (the #284 preamble-cancel-recovery this bug rides on top
	// of), but there is no live runCtx left to run it on either, so the
	// recovered replacement fails the exact same way a turn later —
	// "accepted, reported queued, never executed", the same P0-A/P0-B shape
	// this whole mailbox exists to prevent. This was #307.
	//
	// The fix: return nil here instead of falling back to dispatcherCancel.
	// mb.replacement is already recorded under this same lock, which
	// is sufficient — nothing needs to be cancelled, because nothing is
	// running. The loop's own testLoopRearmSeam window (agent.go) is where
	// the replacement is reclaimed: see reclaimReplacementOrKeep, called
	// there immediately before each iteration's beginGeneration, which
	// atomically swaps the loop's stale `call` for a same-window replacement
	// instead of letting the stale call run first. The caller
	// (sessionAgent.InterruptAndReplace) already treats a nil returned
	// CancelFunc as "nothing to invoke" (`if cancelFn != nil { cancelFn() }`)
	// and still reports hadOwner=true, so the caller-visible contract
	// ("accepted, will run next, do not start a fresh Run()") is unchanged —
	// only WHAT gets cancelled (nothing, correctly, instead of the whole
	// dispatcher) changes.
	return mb.current.cancel, true
}

// reclaimReplacementOrKeep is called by Run's turn loop (agent.go)
// immediately before each iteration's beginGeneration call — the exact
// point testLoopRearmSeam already parks a test at (see that seam's own doc)
// — to atomically check whether an interrupt landed in the inter-turn
// window (#307) and recorded a fresher mb.replacement than the `call` the
// loop is about to run. If so, the replacement is consumed and returned
// instead, so the interrupt's intent ("run this now, not the old one")
// actually takes effect on the very next generation rather than being
// stranded until some later turn's own drain happens to notice it.
//
// Without this, interruptAndReplace's #307 fix (recording mb.replacement
// but returning a nil cancel instead of firing dispatcherCancel) would only
// be half the fix: the loop's local `call` variable was already decided by
// the PREVIOUS turn's own drain before this window began, so simply not
// cancelling anything would let that stale call run to completion first —
// an entire extra provider turn against the ALREADY-SUPERSEDED prompt —
// before the replacement is picked up by that stale turn's own end-of-turn
// drain (drainOrReleaseFinal checks mb.replacement first, ahead of
// submitted/legacy). That is not "replace", it is "queue behind" — the
// caller asked to interrupt-and-replace and would silently get run-then-
// replace instead. Consuming the replacement HERE, before the stale call is
// ever handed to beginGeneration/runTurn, makes replace actually mean
// replace.
//
// `call` is NOT discarded on the replacement-hit branch (closing review
// after the first draft of this fix, same class as #283/P0-2 — "the
// interrupt destroys the very message it's supposed to queue behind"):
// `call` at this point is not an abstraction. On every iteration after the
// first, it is a SessionAgentCall a PRIOR drain (drainOrRelease/
// drainOrReleaseFinal) already popped out of mb.submitted (or the legacy
// queue) specifically because "next" meant "run this next" — it has not run
// yet, and for the ExistingMessageID path (e.g. `crush sessions inject
// --interrupt`) its DB row already exists with nothing that will ever
// answer it if silently dropped here. On the LOOP'S VERY FIRST iteration,
// `call` is instead the original argument to Run() itself — verified, not
// assumed, by TestRun_InterruptBeforeFirstGeneration_ReplacementRunsExactlyOnce:
// an interrupt landing before turn 1's own beginGeneration is just as real a
// window as the inter-turn one (mb.state is already mbOwned by the time
// Run()'s loop reaches its first testLoopRearmSeam call — see submit()'s own
// doc — only mb.current.cancel is still nil), and this method treats that
// `call` exactly the same way: requeued, not special-cased as "never queued
// anywhere so safe to drop". An earlier version of this method returned only
// the replacement and let `call` vanish — with more than one message already
// queued behind the current turn (submitted = [B, C], drain already popped A
// as `call`), that silently destroyed exactly one message (A) while B and C
// survived untouched in mb.submitted, with the choice of victim decided
// purely by scheduling luck (whether the interrupt landed a moment before or
// after the drain popped A). ClearQueue is documented as "the single
// intentional drop-everything-queued operation" — discarding queued work is
// its job alone, not an accidental side effect of this one.
//
// The fix: push `call` back onto the FRONT of mb.submitted before handing
// back the replacement. This mirrors how drainAfterCancel/drainOrReleaseFinal
// already treat replacement-vs-submitted priority elsewhere in this file —
// replacement runs next, but submitted is never destroyed, only deferred.
// So for the A/B/C scenario above: D (the replacement) runs now, A is
// restored to the front of mb.submitted (order: A, B, C) and is picked up
// by D's own end-of-turn drain, so the FIFO order the caller originally
// queued in is preserved end to end, nothing lost, nothing reordered past
// the one deliberate pre-emption (D jumping ahead of A because the caller
// explicitly asked to interrupt).
//
// This does not appear in mailbox_invariant_test.go's stale-cancel-handle
// table: that table's postcondition (current.cancel == nil) is unrelated to
// this method's job, which is choosing WHICH SessionAgentCall the next
// generation runs — it does not touch state/current/dispatcherCancel at
// all. Covered instead by TestMailbox_ReclaimReplacementOrKeep_* in
// mailbox_test.go.
func (mb *mailbox) reclaimReplacementOrKeep(call SessionAgentCall) SessionAgentCall {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		mb.submitted = append([]SessionAgentCall{call}, mb.submitted...)
		return next
	}
	return call
}

// drainAfterCancel implements design §4: the generation-aware drain called
// by the owner's cancel-handling branch (replacing today's
// messageQueue.PopFront check at the isCancelErr site). It checks
// replacement before submitted, since a replacement is a durable "cancel
// current, then run me next" instruction distinct from a plain queued
// follow-up.
func (mb *mailbox) drainAfterCancel() (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	// Hard-stopped (P0-C): the process is shutting down, so refuse to hand
	// the turn loop another call. This is THE branch that made CancelAll
	// start turn N+1 instead of stopping — it runs immediately after the
	// cancel CancelAll itself caused. Queued work is left in place — it will
	// be durably enqueued when the turn loop exits and Run's deferred
	// abandonOwnershipWithHandoff pops it (session_run_queue row, not a
	// provider turn). The shutdown admission gate (tryAdmitRunWg) refuses
	// any in-process Run during shutdown, and App.Shutdown stops the pump
	// before closing the DB.
	if mb.stopped {
		// Clear the spent generation handle on the way out, like the two hit
		// branches below (closing-review follow-up). This branch returns "no
		// more work" while state is still mbOwned, which is exactly the
		// stale-cancel shape rounds 9-13 found five times: left set, a
		// Cancel landing before Run's abandonOwnership defer catches up
		// invokes an already-spent handle instead of falling through to
		// dispatcherCancel. The window is short — so was every one of those.
		mb.current.cancel = nil
		return SessionAgentCall{}, false
	}

	// Both hit branches below clear mb.current.cancel (round 13 review,
	// the fourth instance of the same shape rounds 11/12 already fixed on
	// drainOrReleaseFinal's two branches): by the time this is called
	// (agent.go's isCancelErr block, right before its own `cancel()` call),
	// mb.current.cancel is the just-cancelled generation's own genCtx
	// cancel func — spent (whether invoked by the external Cancel() that
	// caused isCancelErr, or by this function's own caller a line later)
	// but NOT nil. Left untouched, it defeats Cancel()/InterruptAndReplace()'s
	// current.cancel==nil fallback gate exactly like every prior instance,
	// for the window until the next turn's own beginGeneration —
	// arguably the worst of the four, since this is precisely the "user
	// already cancelled once and wants the replacement instead" path: a
	// second Cancel/interrupt landing in this window would silently no-op
	// while the replacement turn streams anyway. dispatcherCancel is left
	// untouched, same reasoning as the other two fixed branches — it's
	// already the live runCancel, nothing to reset.
	if mb.replacement != nil {
		next := *mb.replacement
		mb.replacement = nil
		mb.current.cancel = nil
		return next, true
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		mb.current.cancel = nil
		return next, true
	}
	// Clear the spent handle on the empty branch too (the sixth instance
	// of the same stale-cancel shape rounds 9-14 fixed one branch at a
	// time). This branch is now reachable from runTurn's preamble
	// cancel-recovery (#284): without the clear, current.cancel survives
	// as a spent handle, defeating Cancel()/InterruptAndReplace()'s
	// nil-check fallback to dispatcherCancel for the window between this
	// return and the caller's abandonOwnership defer.
	mb.current.cancel = nil
	return SessionAgentCall{}, false
}
