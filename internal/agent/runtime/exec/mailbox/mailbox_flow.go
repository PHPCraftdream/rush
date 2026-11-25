package mailbox

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Mailbox message-Inject path (design §5): Inject, the atomic
// busy-check-plus-enqueue InjectIfBusy, and the generation-aware
// DrainInjects. Pure code move from mailbox.go.

// Inject implements design §5: appends msg to the pending-Inject list,
// stamping it with the mailbox's CURRENT generation id at Submit time so
// DrainInjects can later decide which generation(s) are responsible for
// splicing it into prepared.Messages. 0 (no owner) is a valid, meaningful
// stamp.
func (mb *Mailbox) Inject(msg message.Message) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	mb.Injects = append(mb.Injects, PendingInject{
		Msg:        msg,
		AfterGenID: mb.Current.ID,
	})
}

// InjectIfBusy is the atomic busy-check-plus-enqueue operation InjectMessage
// needs (design §5, stage 2.4 of the mailbox migration): it checks whether
// the mailbox currently has an owner and, if so, appends msg to the
// pending-Inject list stamped with the Current Generation id — all under a
// single mb.Mu hold. This closes the P1-1 race where the old code's separate
// IsSessionBusy check and injectQueue.Append allowed the owner to finish and
// release to MbIdle between the two operations, stranding the message in the
// Queue with nobody left to drain it (or, from the other direction,
// duplicating it when the next Run's preamble DB read already included the
// row).
//
// Returns true when the message was queued (session was busy); false when the
// session was idle, in which case the message lives only in the DB and will
// be picked up by the next Run's natural preamble DB read.
//
// This method does NOT change ownership state (state/current/dispatcherCancel
// are untouched) and therefore does NOT appear in mailbox_invariant_test.go's
// postcondition table — that table covers operations that hand work to a turn
// loop or end an era, which InjectIfBusy deliberately does neither.
//
// MbReleasing is treated the same as MbIdle here — unlike IsSessionBusy/
// Submit (gated on OS-lock availability, see the MbReleasing const's doc),
// InjectIfBusy only cares whether a live generation loop will ever call
// DrainInjects again. The CRITICAL INVARIANT in DrainOrReleaseFinal's doc
// guarantees it never will once MbReleasing is entered, so queuing here
// would strand the message in mb.Injects — nobody drains it until some
// unrelated future generation does, risking a duplicate splice on top of
// the natural DB read that already covers the MbIdle case.
func (mb *Mailbox) InjectIfBusy(msg message.Message) bool {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.State != MbOwned {
		return false
	}
	mb.Injects = append(mb.Injects, PendingInject{
		Msg:        msg,
		AfterGenID: mb.Current.ID,
	})
	return true
}

// DrainInjects implements design §5: called by the owner's PrepareStep
// (replacing today's unconditional injectQueue.TakeAll) with the
// generation id of the turn currently preparing. Entries stamped at or
// before genID are due now; entries stamped against a strictly future
// generation id are kept for later (not possible via today's call sites,
// kept for forward-compat per the design doc).
func (mb *Mailbox) DrainInjects(genID uint64) []PendingInject {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	var due, later []PendingInject
	for _, inj := range mb.Injects {
		if inj.AfterGenID <= genID {
			due = append(due, inj)
		} else {
			later = append(later, inj)
		}
	}
	mb.Injects = later
	return due
}

// Mailbox interrupt and cancellation paths: HardStop/IsStopped,
// InterruptAndReplace with its inter-turn ReclaimReplacementOrKeep, and
// the generation-aware DrainAfterCancel. Pure code move from mailbox.go.

// HardStop implements the shutdown half of P0-C: it latches the mailbox
// closed and returns the dispatcher cancel (the DURABLE, whole-Run()
// CancelFunc) plus the Current Generation's cancel, for the caller to
// invoke outside mb.Mu.
//
// Unlike Cancel/InterruptAndReplace — which deliberately target only the
// Current Generation so a turn loop survives to run the Replacement —
// this is the operation that must actually END the dispatcher: cancelling
// dispatcherCancel unwinds Run() itself, and `Stopped` guarantees that on
// the way out no drain hands it another call to run.
//
// Anything still queued is left in place rather than discarded, and since
// task #340 leaving it in place IS a save: when the latch refuses the
// turn-loop drains and Run() unwinds, Run's deferred
// abandonOwnershipWithHandoff pops the Queue and durably enqueues every
// queued call via restartOrphanedWithRetry — a session_run_queue row, not
// a provider turn. (An earlier version of this comment claimed the Queue
// was "genuinely lost" because a queued agentcall.SessionAgentCall is not a DB row;
// that was true pre-#340, when restart meant a detached Run(), but is
// obsolete now.) The only thing the latch itself must prevent is another
// in-process turn: the shutdown admission gate (tryAdmitRunWg) refuses
// any Run during shutdown, and App.Shutdown stops the pump before
// closing the DB, so the durable rows are safely processed later — after
// restart, in a fresh process. This includes interrupt tick's calls (an
// ExistingMessageID for a row `rush sessions Inject` already wrote):
// they ride the same durable-enqueue path as everything else, they just
// additionally already had a DB row.
//
// HardStop landing while state == MbReleasing (#296/P1-C: DrainOrReleaseFinal
// is between dropping mb.Mu to run release() and reacquiring it to finalize)
// needs no special case here: it takes mb.Mu for the single instant needed
// to set the latch and read the two cancel funcs, so it is never blocked by
// the in-flight, lock-free release() call — that is the entire reason that
// call no longer holds mb.Mu. Both funcs read back nil (cleared on entry to
// MbReleasing — see that transition), so the caller's cancel invocations are
// correctly no-ops: there is no live generation left to interrupt, the turn
// loop is already past its provider work and mid lock-teardown. The latch
// itself is still fully effective — but not because anything re-reads it
// at finalize: the finalize step is latch-blind (in effect since #646;
// #652 deleted the dead Stopped branch outright) and ALWAYS drains work
// that raced in during the release() window out as orphaned for
// restartOrphaned's durable enqueue — not lost, and not handed back to
// the (now shutting-down) turn loop — whether or not a HardStop landed.
// The Stopped-specific half lives in DrainOrReleaseFinal's ENTRY check,
// which skips the live branches and keeps hasNext false when HardStop
// lands BEFORE the drain is even called (see its own doc in
// mailbox_ownership.go).
func (mb *Mailbox) HardStop() (dispatcherCancel, gencancel context.CancelFunc) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	mb.Stopped = true
	return mb.DispatcherCancel, mb.Current.Cancel
}

// IsStopped reports whether HardStop has latched this mailbox closed.
func (mb *Mailbox) IsStopped() bool {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	return mb.Stopped
}

// InterruptAndReplace implements design §4: the coordinator's single entry
// point for "interrupt and replace", replacing QueueMessage+Cancel as one
// atomic mailbox operation. When nobody owns the session, it returns
// hadOwner=false and does nothing further (the caller should instead start
// a fresh Run() with call directly). When a turn is in progress, it records
// call as the Replacement to be consumed by the NEXT generation, and returns
// a cancel func the caller can invoke to interrupt the in-flight generation
// (outside the mailbox lock).
func (mb *Mailbox) InterruptAndReplace(call agentcall.SessionAgentCall) (context.CancelFunc, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	return mb.interruptAndReplaceLocked(call)
}

// InterruptAndReplaceIfCurrent is the compare-and-act form used when a
// coordinator first snapshots a non-durable active call. The token check and
// Replacement recording share one mb.Mu critical section, so a newer call
// cannot be cancelled or replaced by a stale snapshot.
func (mb *Mailbox) InterruptAndReplaceIfCurrent(token ActiveCallToken, call agentcall.SessionAgentCall) (context.CancelFunc, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.State != MbOwned || mb.Stopped || token.Call == nil ||
		mb.Epoch != token.Epoch || mb.Current.ID != token.Generation ||
		mb.CurrentCall != token.Call {
		return nil, false
	}
	return mb.interruptAndReplaceLocked(call)
}

func (mb *Mailbox) interruptAndReplaceLocked(call agentcall.SessionAgentCall) (context.CancelFunc, bool) {
	if mb.State != MbOwned || mb.Stopped {
		// Nobody running: behave like a plain Submit that also happens to
		// return "no cancel needed" — the caller (coordinator) then starts
		// a fresh Run() with call directly instead of relying on drain.
		//
		// `Stopped` counts as "nobody running" too (closing review, blocker
		// 2), even while state is still MbOwned. Recording a Replacement on
		// a hard-Stopped mailbox would report success to the caller — and
		// through it "queued" to the web client — for a payload both drains
		// are now guaranteed to refuse, leaving it to be swept into an
		// in-memory Queue by a process that is exiting. Returning false
		// instead routes the caller to its own no-owner path, where Run
		// refuses with ErrAgentShuttingDown so the failure is visible
		// rather than silent.
		//
		// mb.State == MbReleasing (#296/P1-C) also takes this branch, since
		// it fails `mb.State != MbOwned` too: there is no live generation
		// left to interrupt (the turn loop is already past its provider
		// work, mid lock-teardown), and current.cancel/dispatcherCancel are
		// both nil by the time a turn reaches MbReleasing anyway (cleared on
		// the way in — see DrainOrReleaseFinal). The caller's fallback
		// ("start a fresh Run()") is correct here too: DrainOrReleaseFinal's
		// finalize step is what actually decides whether that fresh call
		// gets handed back to THIS turn loop or a brand new Run() truly
		// starts, once release() returns.
		return nil, false
	}
	// Record the Replacement FIRST, under the same lock that is about to cancel
	// the Current Generation. There is no window between "Replacement is recorded"
	// and "Current Generation is cancelled" for an external observer to land in,
	// because both happen before mu is released.
	//
	// Owner: The Replacement is owned by the current session's mailbox in-memory.
	// Durable record: For calls originating from durable Queue (FromDurableQueue=true),
	// the durable row itself remains the authoritative record; the pump will execute it.
	// For non-durable calls, the mailbox Replacement is the only retry path.
	// Cancellation authority: The returned cancel func interrupts the in-flight generation.
	// Terminal transition: The Replacement becomes the next generation's call after cancellation.
	//
	// P0-1 fix (docs/reviews/2026-08-12-post-fix-release-readiness-follow-up.md):
	// calls FROM the durable Queue (call.FromDurableQueue) do NOT set
	// mb.Replacement to avoid double-execution. The durable row itself is
	// already the retry path, and the pump will execute it. Setting mb.Replacement
	// would create a live copy that runs immediately, leaving the durable row
	// pending for a second execution by the pump. For non-durable calls, the
	// mailbox Replacement IS the only retry path, so they are recorded normally.
	//
	// "the pump will execute it" originally meant only the background
	// RunQueuePump's own periodic tick (3s in production) — leaving a real
	// liveness gap for short-lived processes (rush run), since the process
	// routinely exits before that tick ever fires. Task #421/P0-1 closed
	// that gap WITHOUT touching this guard: RunNonInteractive
	// (internal/app/app.go) now calls RunQueuePump.DrainSessionNow
	// synchronously in the same process once the cancelled generation
	// returns, executing the row through the exact same lease/execute path
	// the background tick uses. This function's guard above is still
	// correct and unchanged — it only decides mailbox ownership, not who
	// eventually runs the durable row.
	if !call.FromDurableQueue {
		// A prior Replacement being overwritten here never gets a turn --
		// InterruptAndReplace's whole point is "run THIS instead". If it
		// carried OnQueueResolved (#1036's live in-process waiter), resolve
		// it now with errQueuedTurnNotRun instead of silently discarding the
		// pointer, or that waiter blocks forever (no implicit timeout exists
		// in this chain). Called while still holding mb.Mu: the hook
		// (runAwaitingAdmission's non-blocking channel send) never touches
		// the mailbox back, so there is no reentrancy risk, and every other
		// mutation in this function already happens under the same lock.
		if mb.Replacement != nil {
			// Package L round 2: safety net -- by construction the
			// Replacement slot never holds a Submit-armed call
			// (InterruptAndSend calls own their scope and are skipped by
			// the arm guard), so this is a no-op unless that invariant
			// ever breaks.
			ReleaseQueuedBuildOwnership(mb.Replacement)
			ResolveCallWithError(*mb.Replacement, "this queued turn was superseded by a newer interrupt-and-replace before it could run")
		}
		mb.Replacement = &call
	}
	// #307 (P1-2 follow-up): mb.Current.Cancel == nil while mb.State ==
	// MbOwned is NOT a special case restricted to "before the very first
	// generation" — it is the inter-turn window too: Run's loop clears
	// current.cancel (via the just-finished turn's own drain — every hit
	// branch of DrainOrRelease/DrainOrReleaseFinal/DrainAfterCancel leaves
	// it nil, see mailbox_invariant_test.go) and does not repopulate it
	// until the NEXT iteration's BeginGeneration call. In BOTH windows
	// (pre-first-generation and inter-turn) there is genuinely nothing
	// live to interrupt — the turn loop itself is between provider calls,
	// not inside one.
	//
	// A previous version of this method (round 9 review, MEDIUM-1) fell
	// back to mb.DispatcherCancel here on the theory that SOME cancel must
	// be returned or the Replacement would be silently stranded. That was
	// wrong: dispatcherCancel is runCancel, the whole-Run() context every
	// future turn's genCtx descends from (see the field's own doc — "never
	// the target of an interrupt"). Firing it here doesn't just end the
	// current (nonexistent) generation, it poisons runCtx for every turn
	// the loop will ever create again, including the Replacement's own —
	// the Replacement gets recorded, then the loop's next iteration derives
	// a turnCtx from an already-cancelled runCtx, whose preamble immediately
	// fails with context.Canceled. That failure DOES recover the mailbox's
	// Replacement (the #284 preamble-cancel-recovery this bug rides on top
	// of), but there is no live runCtx left to run it on either, so the
	// recovered Replacement fails the exact same way a turn later —
	// "accepted, reported queued, never executed", the same P0-A/P0-B shape
	// this whole mailbox exists to prevent. This was #307.
	//
	// The fix: return nil here instead of falling back to dispatcherCancel.
	// mb.Replacement is already recorded under this same lock, which
	// is sufficient — nothing needs to be cancelled, because nothing is
	// running. The loop's own TestLoopRearmSeam window (agent_run.go) is where
	// the Replacement is reclaimed: see ReclaimReplacementOrKeep, called
	// there immediately before each iteration's BeginGeneration, which
	// atomically swaps the loop's stale `call` for a same-window Replacement
	// instead of letting the stale call run first. The caller
	// (sessionAgent.InterruptAndReplace) already treats a nil returned
	// CancelFunc as "nothing to invoke" (`if cancelFn != nil { cancelFn() }`)
	// and still reports hadOwner=true, so the caller-visible contract
	// ("accepted, will run next, do not start a fresh Run()") is unchanged —
	// only WHAT gets cancelled (nothing, correctly, instead of the whole
	// dispatcher) changes.
	return mb.Current.Cancel, true
}

// ReclaimReplacementOrKeep is called by Run's turn loop (agent_run.go)
// immediately before each iteration's BeginGeneration call — the exact
// point TestLoopRearmSeam already parks a test at (see that seam's own doc)
// — to atomically check whether an interrupt landed in the inter-turn
// window (#307) and recorded a fresher mb.Replacement than the `call` the
// loop is about to run. If so, the Replacement is consumed and returned
// instead, so the interrupt's intent ("run this now, not the old one")
// actually takes effect on the very next generation rather than being
// stranded until some later turn's own drain happens to notice it.
//
// Without this, InterruptAndReplace's #307 fix (recording mb.Replacement
// but returning a nil cancel instead of firing dispatcherCancel) would only
// be half the fix: the loop's local `call` variable was already decided by
// the PREVIOUS turn's own drain before this window began, so simply not
// cancelling anything would let that stale call run to completion first —
// an entire extra provider turn against the ALREADY-SUPERSEDED prompt —
// before the Replacement is picked up by that stale turn's own end-of-turn
// drain (DrainOrReleaseFinal checks mb.Replacement first, ahead of
// submitted/legacy). That is not "replace", it is "Queue behind" — the
// caller asked to interrupt-and-replace and would silently get run-then-
// replace instead. Consuming the Replacement HERE, before the stale call is
// ever handed to BeginGeneration/runTurn, makes replace actually mean
// replace.
//
// `call` is NOT discarded on the Replacement-hit branch (closing review
// after the first draft of this fix, same class as #283/P0-2 — "the
// interrupt destroys the very message it's supposed to Queue behind"):
// `call` at this point is not an abstraction. On every iteration after the
// first, it is a agentcall.SessionAgentCall a PRIOR drain (DrainOrRelease/
// DrainOrReleaseFinal) already popped out of mb.Submitted (or the legacy
// Queue) specifically because "next" meant "run this next" — it has not run
// yet, and for the ExistingMessageID path (e.g. `rush sessions Inject
// --interrupt`) its DB row already exists with nothing that will ever
// answer it if silently dropped here. On the LOOP'S VERY FIRST iteration,
// `call` is instead the original argument to Run() itself — verified, not
// assumed, by TestRun_InterruptBeforeFirstGeneration_ReplacementRunsExactlyOnce:
// an interrupt landing before turn 1's own BeginGeneration is just as real a
// window as the inter-turn one (mb.State is already MbOwned by the time
// Run()'s loop reaches its first TestLoopRearmSeam call — see Submit()'s own
// doc — only mb.Current.Cancel is still nil), and this method treats that
// `call` exactly the same way: requeued, not special-cased as "never queued
// anywhere so safe to drop". An earlier version of this method returned only
// the Replacement and let `call` vanish — with more than one message already
// queued behind the current turn (submitted = [B, C], drain already popped A
// as `call`), that silently destroyed exactly one message (A) while B and C
// survived untouched in mb.Submitted, with the choice of victim decided
// purely by scheduling luck (whether the interrupt landed a moment before or
// after the drain popped A). ClearQueue is documented as "the single
// intentional drop-everything-queued operation" — discarding queued work is
// its job alone, not an accidental side effect of this one.
//
// The fix: push `call` back onto the FRONT of mb.Submitted before handing
// back the Replacement. This mirrors how DrainAfterCancel/DrainOrReleaseFinal
// already treat Replacement-vs-submitted priority elsewhere in this file —
// Replacement runs next, but submitted is never destroyed, only deferred.
// So for the A/B/C scenario above: D (the Replacement) runs now, A is
// restored to the front of mb.Submitted (order: A, B, C) and is picked up
// by D's own end-of-turn drain, so the FIFO order the caller originally
// queued in is preserved end to end, nothing lost, nothing reordered past
// the one deliberate pre-emption (D jumping ahead of A because the caller
// explicitly asked to interrupt).
//
// This does not appear in mailbox_invariant_test.go's stale-cancel-handle
// table: that table's postcondition (current.cancel == nil) is unrelated to
// this method's job, which is choosing WHICH agentcall.SessionAgentCall the next
// generation runs — it does not touch state/current/dispatcherCancel at
// all. Covered instead by TestMailbox_ReclaimReplacementOrKeep_* in
// mailbox_test.go.
func (mb *Mailbox) ReclaimReplacementOrKeep(call agentcall.SessionAgentCall) agentcall.SessionAgentCall {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.Replacement != nil {
		next := *mb.Replacement
		mb.Replacement = nil
		next.ReplacementHandoff = true
		// B17 fix: apply the SAME Drain-merge rule every other insertion
		// path already applies, instead of a bare prepend -- a bare prepend
		// let a displaced Drain sit alongside an already-queued Drain (or
		// an already-queued real call), and let a displaced real call leave
		// a stale queued Drain in place instead of dropping it.
		mb.Submitted = MergeQueuedCallAtHead(mb.Submitted, call)
		return next
	}
	return call
}

// DrainAfterCancel implements design §4: the generation-aware drain called
// by the owner's cancel-handling branch (replacing today's
// messageQueue.PopFront check at the isCancelErr site). It checks
// Replacement before submitted, since a Replacement is a durable "cancel
// current, then run me next" instruction distinct from a plain queued
// follow-up.
func (mb *Mailbox) DrainAfterCancel() (agentcall.SessionAgentCall, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	// Hard-Stopped (P0-C): the process is shutting down, so refuse to hand
	// the turn loop another call. This is THE branch that made CancelAll
	// start turn N+1 instead of stopping — it runs immediately after the
	// cancel CancelAll itself caused. Queued work is left in place — it will
	// be durably enqueued when the turn loop exits and Run's deferred
	// abandonOwnershipWithHandoff pops it (session_run_queue row, not a
	// provider turn). The shutdown admission gate (tryAdmitRunWg) refuses
	// any in-process Run during shutdown, and App.Shutdown stops the pump
	// before closing the DB.
	if mb.Stopped {
		// Clear the spent generation handle on the way out, like the two hit
		// branches below (closing-review follow-up). This branch returns "no
		// more work" while state is still MbOwned, which is exactly the
		// stale-cancel shape rounds 9-13 found five times: left set, a
		// Cancel landing before Run's AbandonOwnership defer catches up
		// invokes an already-spent handle instead of falling through to
		// dispatcherCancel. The window is short — so was every one of those.
		mb.Current.Cancel = nil
		return agentcall.SessionAgentCall{}, false
	}

	// Both hit branches below clear mb.Current.Cancel (round 13 review,
	// the fourth instance of the same shape rounds 11/12 already fixed on
	// DrainOrReleaseFinal's two branches): by the time this is called
	// (agent.go's isCancelErr block, right before its own `cancel()` call),
	// mb.Current.Cancel is the just-cancelled generation's own genCtx
	// cancel func — spent (whether invoked by the external Cancel() that
	// caused isCancelErr, or by this function's own caller a line later)
	// but NOT nil. Left untouched, it defeats Cancel()/InterruptAndReplace()'s
	// current.cancel==nil fallback gate exactly like every prior instance,
	// for the window until the next turn's own BeginGeneration —
	// arguably the worst of the four, since this is precisely the "user
	// already cancelled once and wants the Replacement instead" path: a
	// second Cancel/interrupt landing in this window would silently no-op
	// while the Replacement turn streams anyway. dispatcherCancel is left
	// untouched, same reasoning as the other two fixed branches — it's
	// already the live runCancel, nothing to reset.
	if mb.Replacement != nil {
		next := *mb.Replacement
		mb.Replacement = nil
		next.ReplacementHandoff = true
		mb.Current.Cancel = nil
		return next, true
	}
	if len(mb.Submitted) > 0 {
		next := mb.Submitted[0]
		mb.Submitted = mb.Submitted[1:]
		mb.Current.Cancel = nil
		return next, true
	}
	// Clear the spent handle on the empty branch too (the sixth instance
	// of the same stale-cancel shape rounds 9-14 fixed one branch at a
	// time). This branch is now reachable from runTurn's preamble
	// cancel-recovery (#284): without the clear, current.cancel survives
	// as a spent handle, defeating Cancel()/InterruptAndReplace()'s
	// nil-check fallback to dispatcherCancel for the window between this
	// return and the caller's AbandonOwnership defer.
	mb.Current.Cancel = nil
	return agentcall.SessionAgentCall{}, false
}
