package mailbox

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

// Mailbox ownership lifecycle: Submit, DrainOrRelease and the release-
// aware DrainOrReleaseFinal, AbandonOwnership, and the manual-compaction
// BeginRelease/FinishRelease pair. Pure code move from mailbox.go.

// Submit implements design §3: replaces both tryReserveSession +
// activeRequests.Set (the "am I the new owner" path) and
// messageQueue.Append (the "Queue behind the current owner" path) as one
// atomic operation. Returns true when the caller becomes the new owner and
// must run call itself; false when call was appended to the Queue for the
// current owner to drain. When becomeOwner is true, Epoch is this NEW
// ownership era's id — the caller must present it to every DrainOrRelease
// call it makes for the lifetime of its ownership (BLOCKER-2, see the
// Epoch field's doc). Epoch is meaningless (0) when becomeOwner is false:
// the caller never held ownership and has nothing to release.
func (mb *Mailbox) Submit(call agentcall.SessionAgentCall, dispatcherCancel context.CancelFunc) (becomeOwner bool, Epoch uint64) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.State == MbIdle {
		mb.State = MbOwned
		mb.DispatcherCancel = dispatcherCancel
		mb.Epoch++
		copy := call
		mb.CurrentCall = &copy
		return true, mb.Epoch // caller (Run) becomes the new owner, runs call itself
	}
	// mb.State == MbOwned OR mb.State == MbReleasing both land here (#296/
	// P1-C): MbReleasing means release()'s disk I/O is in flight with mb.Mu
	// NOT held, so the OS lock is not actually free yet even though a turn
	// loop isn't either — becoming owner here would race a concurrent
	// TryAcquireSessionLock against this call's own in-flight release. See
	// the MbReleasing const's doc.
	//
	// This call is NOT stranded once release() returns, but the fate
	// depends on WHICH state it landed in:
	//   - MbOwned: the current turn loop is still genuinely alive and its
	//     own end-of-turn drain (DrainOrRelease/DrainOrReleaseFinal) picks
	//     this call up as its next turn in the normal way.
	//   - MbReleasing: by the time release() returns there is no turn loop
	//     left to hand this to — the caller that was releasing is already
	//     on its way out. DrainOrReleaseFinal's Case 4 (see its own doc)
	//     reports this call as orphaned, and drainOrReleaseMerged restarts
	//     it via restartOrphaned — a detached durable-Queue enqueue (task
	//     #340), NOT a handoff to any "still-live" loop.
	//
	// P0-1 fix (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
	// calls FROM the durable Queue (call.FromDurableQueue) do NOT get enqueued
	// in mb.Submitted here. The durable row itself is already the retry path,
	// and the pump will re-lease it after its backoff expires (see
	// RunQueuePump.executeEntry's ErrCallQueuedNotExecuted handling).
	// Enqueuing here would create a double-execution hazard: the live owner
	// would execute the mb.Submitted copy, then after backoff the pump would
	// execute the same durable row again independently. For non-durable calls,
	// the mailbox Queue IS the only retry path, so they are enqueued normally.
	// R1-4: a fail-fast call must NEVER Queue — the whole point of the
	// contract is that the caller learns "someone else owns this session"
	// instead of silently waiting behind it. The decision happens inside
	// THIS critical section (the same mb.Mu hold as the MbIdle check
	// above), so two calls starting simultaneously on an idle mailbox
	// cannot both observe idle: exactly one becomes owner, the loser sees
	// state != MbIdle here and returns without queueing.
	if call.FailIfSessionBusy {
		return false, 0 // caller reports ErrSessionBusy instead of queueing
	}
	if !call.FromDurableQueue {
		// Package L round 2: arm the retained build ownership ONLY here --
		// the moment the mailbox accepts the call into submitted -- never
		// before the reservation. Winners, lock-busy refusals and
		// admission-setup failures never arm (round-1 flaw A). A non-Drain
		// call is never dropped by mergeQueuedCall (it only filters queued
		// Drains), so an armed call is always the one enqueued.
		if !call.IsDrain && mb.ArmQueuedBuild != nil {
			mb.ArmQueuedBuild(&call)
		}
		mb.Submitted = MergeQueuedCall(mb.Submitted, call)
	}
	return false, 0 // caller queues and returns nil, exactly like today
}

// DrainOrRelease implements design §3: called by the owner at the exact
// point today's code calls messageQueue.PopFront at the end of a turn. If
// anything is queued, it is returned and ownership stays with the caller
// (state remains MbOwned). Otherwise ownership is atomically released
// (state flips to MbIdle) in the SAME critical section as the emptiness
// check, closing the P0-3 lost-wakeup window.
//
// Epoch must be the value Submit() returned when granting the caller
// ownership (BLOCKER-2). If the mailbox's current Epoch has since moved on
// — a different, later caller became owner, e.g. because THIS call is a
// stale duplicate release from Run's unconditional cleanup defer running
// after runTurn's own drain already ended the era — this is a safe no-op:
// it must not touch submitted/state/current, which belong to that later
// owner now.
func (mb *Mailbox) DrainOrRelease(Epoch uint64) (agentcall.SessionAgentCall, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return agentcall.SessionAgentCall{}, false
	}
	if len(mb.Submitted) > 0 {
		next := mb.Submitted[0]
		mb.Submitted = mb.Submitted[1:]
		// Same stale-cancel shape as DrainOrReleaseFinal's/DrainAfterCancel's
		// equivalent branches (rounds 11-13) — this function has no live
		// production caller today (its only wrapper, releaseSessionReservation,
		// has zero call sites), but its own doc explicitly reserves it for a
		// future caller with no OS lock in play, so it gets the same
		// defensive clear rather than being a silent fifth instance waiting
		// to be reintroduced.
		mb.Current.Cancel = nil
		return next, true // caller runs another turn; state stays MbOwned
	}
	// Nothing queued AT THE INSTANT OF THIS CHECK, and — because mu is
	// held — nothing CAN be queued between this check and the state flip
	// below.
	if mb.TestDrainSeam != nil {
		mb.TestDrainSeam()
	}
	mb.State = MbIdle
	mb.Current.Cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.DispatcherCancel = nil
	return agentcall.SessionAgentCall{}, false
}

// DrainOrReleaseFinal implements round 11 review, HIGH-1: it replaces the
// former two-step "release to MbIdle under mb.Mu, THEN — outside mb.Mu, in
// drainOrReleaseMerged — separately check the legacy messageQueue and only
// THEN release the OS session lock much later, once Run's whole call stack
// unwinds" shape with one atomic operation, so that "mb.State == MbIdle"
// (observable in-process via IsSessionBusy/Submit/CancelAll/IsBusy, all of
// which just read mb.State under mb.Mu) can no longer become true while the
// OS-level session.SessionLock this same call is still holding is not yet
// released.
//
// Without this, a same-process caller that saw IsSessionBusy(sessionID) ==
// false and tried to become the new owner via Submit() — legitimately, that
// IS what "not busy" is supposed to mean — could reach
// session.TryAcquireSessionLock and get a spurious SessionLockBusyError from
// its own process's prior turn, which hadn't finished unwinding
// (runTurn's deferred wg.Wait() for title generation, then Run's own
// deferred lk.Release()) yet. Because tryReserveSession's "someone already
// owns it" branch never re-queues (Submit()'s owner-branch assumes the
// eventual real owner will drain it), that manifested as a silently
// dropped user message, not a retryable error.
//
// release is called ONLY when BOTH mb.Submitted came back empty — i.e.
// exactly the branch that used to flip mb.State to MbIdle directly. As of
// #296/P1-C it is called WITHOUT mb.Mu held (see the MbReleasing const's doc
// for the full state-machine rationale): mb.State is set to MbReleasing and
// mb.Mu is released before invoking it, then mb.Mu is reacquired to finalize
// once it returns. It must release the OS-level session lock (or be a no-op
// when a.dataDir == "" and no lock was ever acquired — see
// drainOrReleaseMerged's call site). Any error it returns is surfaced to the
// caller for logging; it never blocks the state flip to MbIdle, mirroring
// how Run's own `defer lk.Release()` today only logs a Release failure rather
// than treating it as fatal — a failed unlock must not leave the mailbox
// permanently non-MbIdle with nobody left to retry it. A panic escaping
// release() is recovered (in the same way) and turned into an error for the
// same reason: the mailbox must always reach a terminal state, never get
// stuck in MbReleasing.
//
// Priority of branch evaluation within this function is now explicitly
// "Replacement -> submitted -> release" (round 14 review, P0-A
// fix), closing a race where a late interrupt whose cancel lost to
// Stream's normal completion could strand the Replacement: mb.Replacement
// was set by InterruptAndReplace under mb.Mu, but agent.Stream returned
// nil (not context.Canceled), so runTurn took the normal-completion path
// instead of the isCancelErr branch's DrainAfterCancel. The Replacement
// check here ensures the Replacement is recovered without releasing the
// OS lock, preserving the same atomic state machine semantics
// DrainAfterCancel already provides for the cancel path.
//
// One accepted trade-off from round 12 review, recorded here rather than
// left implicit:
//   - The OS lock hand-off point moved earlier than it used to be: before
//     this function existed, the lock stayed held through the REST of
//     runTurn's own deferred cleanup (stream watchdog stop, waiting on the
//     title-generation goroutine) and Run's own trailing defers. Now it is
//     released the instant both queues are confirmed empty, so a title
//     rename (sessions.Rename, including its context.WithoutCancel fallback)
//     or a final cost increment can still be in flight AFTER a different
//     process has already acquired the lock for the same session. Both
//     writes are narrowly-scoped, additive/idempotent SQL updates (not
//     read-modify-write on shared state), so the worst case is a cosmetic
//     title race, not data loss — but it is a real, deliberate narrowing of
//     what the OS lock's hold-time used to cover.
//
// #296/P1-C moves release() (a real syscall: unlock, close, and — via
// session.SessionLock.Release — a metadata truncate/remove) OUTSIDE mb.Mu:
// it used to run WHILE mb.Mu was held, so disk I/O briefly blocked every
// other in-process reader of this mailbox (IsSessionBusy, IsBusy, CancelAll,
// Cancel, QueuedPrompts) for that session — an unbounded control-plane stall
// if the filesystem/AV/SMB share hung, since release() has no context and no
// timeout. mb.State == MbReleasing stands in for the mutex as far as HIGH-1's
// atomicity goes (see the MbReleasing const's doc for why every other
// mutator already does the right thing around it).
//
// CRITICAL INVARIANT, closing brief #297 (a same-process reviewer's finding
// against an earlier version of this fix): once this call has entered
// MbReleasing, it can NEVER return hasNext=true / state==MbOwned again, no
// matter what lands in mb.Submitted/mb.Replacement during the release()
// window. release() has, by the time the finalize step below runs, already
// been invoked — successfully, with an error, or via a recovered panic — and
// there is no "un-release" operation: the OS lock this call was holding is
// either genuinely gone or its fate is unknown, either way NOT something
// this call can hand to a turn loop as "still holding it for you". An
// earlier draft of this function re-armed MbOwned and returned hasNext=true
// for exactly this case, on the theory that the turn loop still on the stack
// in runTurn could just keep going — but the turn loop's authority to run
// AT ALL came from that now-released lock; running another turn under it is
// indistinguishable, to a second process racing to acquire the same lock,
// from two owners of one session. A Submit()/InterruptAndReplace() landing
// in the release() window still correctly queues rather than becoming owner
// (MbReleasing reads as busy — see the MbReleasing const's doc), but the
// finalize step's job is to DRAIN that queued work OUT and hand it to the
// caller as orphaned, not to resurrect ownership for it. See orphaned's own
// doc below and drainOrReleaseMerged's doc in agent.go for how the caller is
// required to run it (a fresh, independent Run()-equivalent acquisition,
// mirroring coordinator.startDetachedRun's existing P0-B contract) instead
// of continuing the current turn loop on a lock that is no longer held.
func (mb *Mailbox) DrainOrReleaseFinal(
	Epoch uint64,
	release func() error,
) (call agentcall.SessionAgentCall, hasNext bool, orphaned []agentcall.SessionAgentCall, releaseErr error) {
	mb.Mu.Lock()

	if mb.Epoch != Epoch {
		mb.Mu.Unlock()
		return agentcall.SessionAgentCall{}, false, nil, nil
	}
	// Hard-Stopped (P0-C): end the era rather than continuing into another
	// turn. Fall through to the MbReleasing path below (rather than an early
	// return) so the OS lock is still released and the mailbox still lands
	// on MbIdle — a shutdown must not leave the session marked busy with the
	// lock held. Queued work is NOT returned as "hasNext=true" for another
	// turn loop (that would start a provider turn during shutdown), but it
	// IS handed out as orphaned in the finalize step so drainOrReleaseMerged
	// durably enqueues it via restartOrphaned — see the finalize step's doc.
	if !mb.Stopped {
		// Seam for deterministic test interleaving: fires right after the Epoch check,
		// before ANY Queue (Replacement, submitted) is consulted — this is the
		// earliest deterministic pause point. Existing tests that relied on the seam
		// firing before the legacy-Queue check are unaffected because their scenarios
		// now Queue into mb.Submitted instead, so the flow still reaches the same
		// submitted/release branches after the seam.
		if mb.TestDrainSeam != nil {
			mb.TestDrainSeam()
		}
		// P0-A fix: a late interrupt whose cancel lost the race to Stream's normal
		// completion lands here. mb.Replacement was recorded by InterruptAndReplace
		// under mb.Mu, but agent.Stream returned nil (not context.Canceled), so runTurn
		// took the normal-completion path instead of the isCancelErr branch's
		// DrainAfterCancel. Without this check, the Replacement is silently stranded:
		// the drain releases ownership and the OS lock, and Run's defer
		// can only no-op it under a new era. Priority "Replacement ->
		// submitted -> release" is now ONE atomic state machine operation,
		// matching DrainAfterCancel's existing ordering. mb.Current.Cancel = nil — same invariant as every other "keep running"
		// branch (see mailbox_invariant_test.go). State stays MbOwned, OS lock untouched,
		// Epoch NOT bumped — same semantics as the existing keep-running branches.
		if mb.Replacement != nil {
			next := *mb.Replacement
			mb.Replacement = nil
			next.ReplacementHandoff = true
			mb.Current.Cancel = nil
			mb.Mu.Unlock()
			return next, true, nil, nil
		}
		if len(mb.Submitted) > 0 {
			next := mb.Submitted[0]
			mb.Submitted = mb.Submitted[1:]
			// mb.Current.Cancel must be cleared here too (round 12 review,
			// finding A — the SAME MEDIUM-1 shape as the former checkLegacy
			// branch, just on the more commonly hit mb.Submitted path): by
			// the time this branch runs, it still holds the JUST-FINISHED
			// turn's own genCtx cancel — already invoked once via runTurn's
			// unconditional `cancel()` call right before this drain — inert
			// but NOT nil, which defeats Cancel()/InterruptAndReplace()'s
			// current.cancel==nil fallback gate exactly like the original
			// defect, for the whole window until the next turn's own
			// BeginGeneration (which can be as long as title generation
			// takes).
			// dispatcherCancel is left untouched here (unlike the release
			// branch below): it's already the live runCancel from Submit()/the
			// loop's own re-arm, still valid for the caller's own remaining
			// lifetime — nothing to reset.
			mb.Current.Cancel = nil
			mb.Mu.Unlock()
			return next, true, nil, nil // caller runs another turn; state stays MbOwned, lock stays held
		}
	}

	// RELEASING (#296/P1-C): reached when Stopped was already latched at
	// entry, OR when Replacement/submitted all came back empty —
	// exactly the condition that used to flip mb.State straight to MbIdle
	// while still holding mb.Mu across release()'s disk I/O. MbReleasing is
	// published and mb.Mu is dropped BEFORE calling release(): every other
	// mutator's existing gate treats MbReleasing as busy/no-live-generation
	// (see the MbReleasing const's doc for the full per-method rationale),
	// which is what keeps HIGH-1 intact without holding the mutex over I/O
	// with no context and no timeout.
	//
	// current.cancel and dispatcherCancel are cleared HERE, on entry to
	// MbReleasing, not left for the finalize step below: HardStop() and
	// Cancel()/InterruptAndReplace() can all run while release() is in
	// flight (they only need mb.Mu for an instant, never blocked by the
	// in-flight I/O — that is the entire point of this state), and their
	// documented MbReleasing behavior (a no-op interrupt, since there is no
	// live generation left to cancel) depends on finding both nil. current.id
	// is preserved (monotonic, see the generation field's doc) — only the
	// spent cancel funcs are cleared, mirroring every other "era transition"
	// branch above.
	mb.Current.Cancel = nil
	mb.DispatcherCancel = nil
	mb.State = MbReleasing
	mb.Mu.Unlock()

	// release() runs with NO mailbox lock held. A panic is recovered into an
	// error so the finalize step below always runs — the mailbox must never
	// get stuck in MbReleasing, whether release() errors or panics.
	if release != nil {
		releaseErr = CallReleaseRecoveringPanic(release)
	}

	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	// FINALIZE. release() has returned (or panicked, now folded into
	// releaseErr). This step ALWAYS lands on MbIdle — see this function's own
	// doc for why #296/P1-C's hand-back-to-MbOwned shape (an earlier draft of
	// this fix) was wrong: release() has already run by the time we get here,
	// so there is no OS lock left to keep running a turn loop under.
	mb.State = MbIdle
	mb.Current.Cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.DispatcherCancel = nil

	// The finalize step deliberately treats a Stopped and a non-Stopped
	// mailbox identically: any work that raced into the mailbox (via
	// CancelAll's HardStop latching Stopped mid-release, or via a plain
	// Submit()/InterruptAndReplace during the release() window) is drained
	// out below and handed to the caller as orphaned, NOT run as another
	// in-process turn. The shutdown semantics — hasNext stays false so a
	// shutdown never starts another provider turn — live entirely in the
	// ENTRY check above, which skips the live branches for a Stopped
	// mailbox. drainOrReleaseMerged durably enqueues the orphaned work via
	// restartOrphaned — a DB row, not a provider turn ("restart" Stopped
	// being a fresh provider turn in task #340) — exactly matching the
	// abandon path's behavior on a latched mailbox (task #641 / twelfth
	// review N-3). The shutdown admission gate (tryAdmitRunWg) refuses any
	// Run during shutdown, and App.Shutdown stops the pump before closing
	// the DB, so the durable row is safely processed later.
	//
	// Historical note: an explicit `if mb.Stopped { ... }` branch used to
	// live here (before #646 it discarded the raced-in work; #646 changed
	// it to enqueue). Task #652 (thirteenth review P3-3) deleted it once
	// it became byte-identical to this fall-through.

	// Drain everything that raced into the mailbox during the release()
	// window (a Submit() or a recovered InterruptAndReplace observed
	// MbReleasing as "not idle" and queued rather than became owner — see
	// the MbReleasing const's doc) OUT of the mailbox and hand it to the
	// caller as orphaned, instead of resurrecting MbOwned for it.
	//
	// Order (Replacement, then submitted FIFO) mirrors the priority the
	// live branches above already use, purely so the orphaned slice's order
	// matches what a live turn loop would have run first — orphaned calls
	// are each restarted independently (see this function's own doc and
	// drainOrReleaseMerged's), so the order has no atomicity consequence,
	// only a "if you can only run one right now, run this one first" one.
	if mb.Replacement != nil {
		orphaned = append(orphaned, *mb.Replacement)
		mb.Replacement = nil
	}
	if len(mb.Submitted) > 0 {
		orphaned = append(orphaned, mb.Submitted...)
		mb.Submitted = nil
	}

	// This is the ONLY point MbIdle becomes observable, strictly AFTER the
	// OS lock was released (or release() failed/panicked — releaseErr is
	// surfaced for logging, but a failed unlock must not leave the mailbox
	// permanently non-idle with nobody left to retry it, mirroring how a
	// failed Run() defer lk.Release() today only logs). This is HIGH-1: no
	// same-process observer could have acted on "session is free" before
	// this line runs, because every observer's read (IsSessionBusy, IsBusy,
	// Submit, InterruptAndReplace, Cancel) treated MbReleasing as busy for
	// the whole window between the unlock above and this relock — and
	// MbIdle now means exactly that: no in-process owner, AND the OS lock is
	// free. orphaned calls are not exempt: the caller must acquire a FRESH
	// OS lock (a fresh Run(), or the coordinator.startDetachedRun-equivalent
	// restart) for each of them, exactly as if a brand new caller had
	// submitted them against an idle mailbox — because that is, at this
	// instant, genuinely what they are.
	return agentcall.SessionAgentCall{}, false, orphaned, releaseErr
}

// AbandonOwnership implements Run's cleanup-defer release path (round 9
// review, BLOCKER-2a). It is NOT the same operation as DrainOrRelease:
// DrainOrRelease is called by an owner's OWN live turn loop, which can
// choose to keep running ("found something queued, stay owned, run it as
// the next turn"). Run's defer calls this instead specifically because it
// has NO live turn loop left — it fires on every return from Run(),
// including early bail-outs (OS-lock acquisition failure) and any
// early-return inside runTurn that skipped runTurn's own final drain (an
// error path). In both cases there is nobody left to hand a "keep
// running" answer to, so — unlike DrainOrRelease — this ALWAYS ends the
// era at MbIdle.
//
// Unlike the pre-#308 version, this method does NOT drain submitted or
// Replacement out to the caller. Entries left in submitted survive in
// place for the next owner to drain via DrainOrReleaseFinal, and any
// pending Replacement is folded INTO submitted (so it becomes a normal
// queued follow-up rather than pre-empting the next owner's first turn).
// The former messageQueue re-Queue that the caller used to perform is no
// longer needed: the mailbox's own submitted Queue is now the single
// source of truth for queued work.
//
// Epoch behaves exactly as in DrainOrRelease: a mismatch means this era
// already ended (a concurrent Submit became the new owner, or
// DrainOrReleaseFinal already released it) — a safe no-op that touches
// nothing, since whatever is in submitted/Replacement now belongs to
// that later owner.
func (mb *Mailbox) AbandonOwnership(Epoch uint64) (hadWork bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return false
	}
	// Fold any pending Replacement into submitted so the next owner
	// treats it as a normal queued follow-up, not as a pre-emption
	// target for ReclaimReplacementOrKeep. A Replacement left in
	// mb.Replacement across an era boundary would silently jump the
	// Queue of whatever the next owner's first turn happens to be.
	if mb.Replacement != nil {
		mb.Submitted = append(mb.Submitted, *mb.Replacement)
		mb.Replacement = nil
	}
	hadWork = len(mb.Submitted) > 0
	mb.State = MbIdle
	mb.Current.Cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.DispatcherCancel = nil
	return hadWork
}

// BeginRelease atomically transitions the mailbox to MbReleasing state
// for a manual compaction. This mirrors the first half of DrainOrReleaseFinal's
// MbReleasing transition pattern, allowing manual compaction to signal
// "OS lock release is in progress" separately from "still actively streaming".
//
// P1-2 fix (2026-08-09): Manual compaction used to call lk.Release() directly
// without any MbReleasing visibility (mailbox stayed MbOwned during the release).
// This meant that a hung filesystem/AV/SMB during lk.Release() would leave the
// mailbox permanently MbOwned, indistinguishable from an actively streaming turn.
// With MbReleasing visibility, diagnostic tools and future code can distinguish
// "releasing OS lock" from "still working".
//
// Returns true on successful transition, false if Epoch mismatch (era already ended)
// or state cannot transition (already MbReleasing, MbIdle, etc.).
//
// The caller must:
//  1. Call BeginRelease(Epoch) to get MbReleasing visibility.
//  2. Release mb.Mu (done by this method).
//  3. Call the actual release callback (e.g., lk.Release()).
//  4. Call FinishRelease(Epoch) to transition to MbIdle.
//
// This ensures the same "MbReleasing without holding mb.Mu over I/O" invariant
// that DrainOrReleaseFinal maintains.
func (mb *Mailbox) BeginRelease(Epoch uint64) bool {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return false
	}
	// Can only transition from MbOwned to MbReleasing.
	// If already MbReleasing, MbIdle, or some other state, this is a no-op.
	if mb.State != MbOwned {
		return false
	}

	// Clear the cancel handles before entering MbReleasing, matching DrainOrReleaseFinal's
	// pattern. This ensures Cancel()/InterruptAndReplace() correctly treat MbReleasing
	// as "no live generation left to cancel".
	mb.Current.Cancel = nil
	mb.DispatcherCancel = nil
	mb.State = MbReleasing
	return true
}

// FinishRelease completes the release transition, moving from MbReleasing to MbIdle.
// This must be called after the actual release callback (e.g., lk.Release()) completes.
// Returns true on successful transition to MbIdle, false if Epoch mismatch or state
// was not MbReleasing (stale/racing call).
func (mb *Mailbox) FinishRelease(Epoch uint64) bool {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return false
	}
	// Must be in MbReleasing state to complete the transition.
	if mb.State != MbReleasing {
		return false
	}
	mb.State = MbIdle
	return true
}

// Mailbox submitted-Queue primitives: Queue, the pop accessors, the
// Epoch-guarded abandon-and-pop combinations, and ClearAll. Pure code
// move from mailbox.go.

// ClearAll implements design §4's "ClearQueue is the one intentional
// drop-everything operation": clears submitted, Replacement, and Injects
// together under mu. It does NOT release ownership (state/current/
// dispatcherCancel are untouched) — the owner is still running and merely
// wants its pending queues discarded, not its reservation yanked.
//
// Any dropped call carrying OnQueueResolved (#1036's live in-process waiter)
// is resolved with errQueuedTurnNotRun before this returns — ClearQueue
// discards the call outright (unlike the orphan/restart path, there is no
// durable row to fall back on), so silently dropping it would leave its
// waiter blocked forever. Resolution runs AFTER mb.Mu is released: the hook
// itself never touches the mailbox, but there is no reason to hold the lock
// across an arbitrary caller-supplied callback.
func (mb *Mailbox) ClearAll() {
	mb.Mu.Lock()
	dropped := mb.Submitted
	Replacement := mb.Replacement
	mb.Submitted = nil
	mb.Replacement = nil
	mb.Injects = nil
	mb.Mu.Unlock()

	const reason = "the session's queue was explicitly cleared"
	// Package L round 2: close any retained queued-call build scope on the
	// way out (release is idempotent; no-op for unmarked calls).
	ReleaseQueuedCallsBuildOwnership(dropped)
	ResolveCallsWithError(dropped, reason)
	if Replacement != nil {
		ReleaseQueuedBuildOwnership(Replacement)
		ResolveCallWithError(*Replacement, reason)
	}
}

// Queue appends call to the submitted Queue regardless of ownership state.
// Used by QueueMessage (the fire-and-forget Queue primitive) and by the
// re-Queue paths in Run's defer and runSummarize that used to target the
// legacy messageQueue. When the session is idle, the entry survives in
// submitted until the next Submit() becomes owner and drains it via
// DrainOrReleaseFinal.
func (mb *Mailbox) Queue(call agentcall.SessionAgentCall) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	mb.Submitted = MergeQueuedCall(mb.Submitted, call)
}

// PopFirstSubmitted removes and returns the first entry from the submitted
// Queue, regardless of mailbox state. No production caller uses this
// directly today: runSummarize's manual-compaction success path moved to
// the atomic AbandonOwnershipAndPopFirstSubmitted below (see its own doc),
// which closes a reordering gap a separate AbandonOwnership()+
// PopFirstSubmitted() pair would reopen. Exercised directly by unit tests
// of the Queue mechanics in isolation from ownership state.
func (mb *Mailbox) PopFirstSubmitted() (agentcall.SessionAgentCall, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if len(mb.Submitted) == 0 {
		return agentcall.SessionAgentCall{}, false
	}
	first := mb.Submitted[0]
	mb.Submitted = mb.Submitted[1:]
	return first, true
}

// AbandonOwnershipAndPopSubmitted atomically releases mailbox ownership and
// returns ALL entries currently in the submitted Queue. This is the atomic
// combination of AbandonOwnership() and PopAllSubmitted() that closes the
// era-boundary reordering gap described in PopAllSubmitted's doc comment.
//
// Epoch must be the value Submit() returned when granting the caller ownership.
// If the mailbox's current Epoch has since moved on — a different, later caller
// became owner — this is a safe no-op that returns nil and touches nothing,
// since whatever is in submitted now belongs to that later owner.
//
// This method deliberately does NOT consult mb.Stopped: the hard-stop
// latch's job is to stop TURN-LOOP drains from handing a shutting-down
// process another provider turn (DrainAfterCancel, DrainOrReleaseFinal's
// ENTRY check — since #646 its finalize step is latch-blind in effect —
// and InterruptAndReplace). Popping the Queue here only feeds
// restartOrphanedWithRetry, which durably enqueues to the run Queue
// rather than starting a turn — and any later pump execution is refused
// by the shutdown admission gate (task #641 F-4; an earlier doc claimed
// "every drain refuses on the latch", which was never true of this path).
//
// This method holds mb.Mu for the entire operation: it first checks the Epoch,
// then folds any pending Replacement into submitted, flips state to MbIdle,
// clears current.cancel/dispatcherCancel, and finally copies and clears the
// submitted Queue — all without releasing mb.Mu in between. This ensures that
// no concurrent Submit() can become the new owner and add work to submitted
// after the Epoch check but before the pop, preventing the race where work from
// a new era would be handed to restartOrphanedWithRetry instead of being left
// for the new owner's own drain.
//
// Returns nil (not an empty slice) when there is no queued work, matching
// PopAllSubmitted's semantics for consistency with existing callers and tests.
func (mb *Mailbox) AbandonOwnershipAndPopSubmitted(Epoch uint64) []agentcall.SessionAgentCall {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return nil
	}
	// Fold any pending Replacement into submitted so the next owner
	// treats it as a normal queued follow-up, not as a pre-emption
	// target for ReclaimReplacementOrKeep. A Replacement left in
	// mb.Replacement across an era boundary would silently jump the
	// Queue of whatever the next owner's first turn happens to be.
	if mb.Replacement != nil {
		mb.Submitted = append(mb.Submitted, *mb.Replacement)
		mb.Replacement = nil
	}
	// Copy the submitted Queue BEFORE clearing it.
	var all []agentcall.SessionAgentCall
	if len(mb.Submitted) > 0 {
		all = make([]agentcall.SessionAgentCall, len(mb.Submitted))
		copy(all, mb.Submitted)
		mb.Submitted = mb.Submitted[:0]
	}
	mb.State = MbIdle
	mb.Current.Cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.DispatcherCancel = nil
	return all
}

// AbandonOwnershipAndPopFirstSubmitted atomically releases mailbox
// ownership and returns the FIRST entry in the submitted Queue, leaving
// any remaining entries in place for the next owner's own end-of-turn
// drain. This is the PopFirstSubmitted counterpart to
// AbandonOwnershipAndPopSubmitted above: runSummarize's manual-compaction
// success path (agent_compaction.go) needs exactly PopFirstSubmitted's existing
// "pop only the first entry, leave the rest queued" semantics, but
// sequencing AbandonOwnership() and PopFirstSubmitted() as two separate
// lock acquisitions there reopened the identical era-boundary reordering
// window AbandonOwnershipAndPopSubmitted was added to close for the
// "pop all" case — found during the closing review of the release-
// readiness round, since P2-5's fix only touched
// abandonOwnershipWithHandoff and this second call site was missed.
//
// Epoch must be the value Submit() returned when granting the caller
// ownership. If the mailbox's current Epoch has since moved on, this is a
// safe no-op that returns (zero value, false) and touches nothing.
func (mb *Mailbox) AbandonOwnershipAndPopFirstSubmitted(Epoch uint64) (agentcall.SessionAgentCall, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.Epoch != Epoch {
		return agentcall.SessionAgentCall{}, false
	}
	if mb.Replacement != nil {
		mb.Submitted = append(mb.Submitted, *mb.Replacement)
		mb.Replacement = nil
	}
	mb.State = MbIdle
	mb.Current.Cancel = nil
	mb.DispatcherCancel = nil

	if len(mb.Submitted) == 0 {
		return agentcall.SessionAgentCall{}, false
	}
	first := mb.Submitted[0]
	mb.Submitted = mb.Submitted[1:]
	return first, true
}

// PopAllSubmitted removes and returns ALL entries currently in the
// submitted Queue, regardless of mailbox state. Used by
// abandonOwnershipWithHandoff to start detached runs for all work left in
// the mailbox after a non-cancel error.
//
// IMPORTANT: This method does NOT check epochs. If called outside the
// abandonOwnershipWithHandoff path (or any other path that does not
// already hold exclusive control over the mailbox's era), it can return
// work that belongs to a later ownership era. For finalizer paths that
// need Epoch safety, use AbandonOwnershipAndPopSubmitted instead, which
// combines AbandonOwnership and PopAllSubmitted into a single atomic
// operation that checks the Epoch before popping.
//
// The era-boundary reordering gap that this doc comment originally
// described (see the comment preserved below for historical context) is
// now closed by AbandonOwnershipAndPopSubmitted, which abandonOwnershipWithHandoff
// uses exclusively. Callers should NOT manually sequence AbandonOwnership
// followed by PopAllSubmitted, as that would re-open the gap.
//
// HISTORICAL CONTEXT (preserved for reference):
//
// PopFirstSubmitted/PopAllSubmitted remain the only submitted-Queue
// mutators that take no Epoch argument (contrast DrainOrRelease/
// DrainOrReleaseFinal, which reject a stale Epoch). This was previously
// documented as safe because "no new Submit can land on MbIdle — they all
// Queue into submitted under the same lock" — that statement describes
// Submit()'s own behavior correctly, but does NOT cover Queue() (used by
// QueueMessage and the legacy re-Queue paths), which appends to submitted
// unconditionally regardless of mailbox state.
//
// The actual, narrower guarantee: abandonOwnershipWithHandoff calls
// mb.AbandonOwnership(Epoch) and this method as TWO SEPARATE lock
// acquisitions, not one atomic section. Between them, a new Submit() can
// legitimately become the new owner (MbIdle -> MbOwned, Epoch bumped), and
// if that NEW owner's session then receives an unrelated Queue() call
// (e.g. a concurrent QueueMessage for the same session) before this method
// runs, PopAllSubmitted has no Epoch check to exclude it — it will
// scoop up an entry that logically belongs to the new owner's era and
// hand it to restartOrphanedWithRetry instead of leaving it for the new
// owner's own end-of-turn drain.
//
// As of this writing that is a REORDERING defect, not a data-loss one: the
// call still gets a runner (restartOrphanedWithRetry durably enqueues it,
// task #340), just via the detached path instead of the new owner's normal
// turn. If a future change makes that distinction matter (e.g. ordering
// guarantees callers start depending on), give PopAllSubmitted the same
// Epoch parameter DrainOrRelease/DrainOrReleaseFinal already have, rather
// than assuming this doc comment alone keeps the two in sync.
func (mb *Mailbox) PopAllSubmitted() []agentcall.SessionAgentCall {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if len(mb.Submitted) == 0 {
		return nil
	}
	all := make([]agentcall.SessionAgentCall, len(mb.Submitted))
	copy(all, mb.Submitted)
	mb.Submitted = mb.Submitted[:0]
	return all
}
