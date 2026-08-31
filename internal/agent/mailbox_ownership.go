// Mailbox ownership lifecycle: submit, drainOrRelease and the release-
// aware drainOrReleaseFinal, abandonOwnership, and the manual-compaction
// beginRelease/finishRelease pair. Pure code move from mailbox.go.

package agent

import (
	"context"
	"fmt"
)

// submit implements design §3: replaces both tryReserveSession +
// activeRequests.Set (the "am I the new owner" path) and
// messageQueue.Append (the "queue behind the current owner" path) as one
// atomic operation. Returns true when the caller becomes the new owner and
// must run call itself; false when call was appended to the queue for the
// current owner to drain. When becomeOwner is true, epoch is this NEW
// ownership era's id — the caller must present it to every drainOrRelease
// call it makes for the lifetime of its ownership (BLOCKER-2, see the
// epoch field's doc). epoch is meaningless (0) when becomeOwner is false:
// the caller never held ownership and has nothing to release.
func (mb *mailbox) submit(call SessionAgentCall, dispatcherCancel context.CancelFunc) (becomeOwner bool, epoch uint64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.state == mbIdle {
		mb.state = mbOwned
		mb.dispatcherCancel = dispatcherCancel
		mb.epoch++
		return true, mb.epoch // caller (Run) becomes the new owner, runs call itself
	}
	// mb.state == mbOwned OR mb.state == mbReleasing both land here (#296/
	// P1-C): mbReleasing means release()'s disk I/O is in flight with mb.mu
	// NOT held, so the OS lock is not actually free yet even though a turn
	// loop isn't either — becoming owner here would race a concurrent
	// TryAcquireSessionLock against this call's own in-flight release. See
	// the mbReleasing const's doc.
	//
	// This call is NOT stranded once release() returns, but the fate
	// depends on WHICH state it landed in:
	//   - mbOwned: the current turn loop is still genuinely alive and its
	//     own end-of-turn drain (drainOrRelease/drainOrReleaseFinal) picks
	//     this call up as its next turn in the normal way.
	//   - mbReleasing: by the time release() returns there is no turn loop
	//     left to hand this to — the caller that was releasing is already
	//     on its way out. drainOrReleaseFinal's Case 4 (see its own doc)
	//     reports this call as orphaned, and drainOrReleaseMerged restarts
	//     it via restartOrphaned — a detached durable-queue enqueue (task
	//     #340), NOT a handoff to any "still-live" loop.
	//
	// P0-1 fix (docs/reviews/2026-08-11-release-readiness-concurrency-and-code-review.md):
	// calls FROM the durable queue (call.FromDurableQueue) do NOT get enqueued
	// in mb.submitted here. The durable row itself is already the retry path,
	// and the pump will re-lease it after its backoff expires (see
	// RunQueuePump.executeEntry's ErrCallQueuedNotExecuted handling).
	// Enqueuing here would create a double-execution hazard: the live owner
	// would execute the mb.submitted copy, then after backoff the pump would
	// execute the same durable row again independently. For non-durable calls,
	// the mailbox queue IS the only retry path, so they are enqueued normally.
	// R1-4: a fail-fast call must NEVER queue — the whole point of the
	// contract is that the caller learns "someone else owns this session"
	// instead of silently waiting behind it. The decision happens inside
	// THIS critical section (the same mb.mu hold as the mbIdle check
	// above), so two calls starting simultaneously on an idle mailbox
	// cannot both observe idle: exactly one becomes owner, the loser sees
	// state != mbIdle here and returns without queueing.
	if call.FailIfSessionBusy {
		return false, 0 // caller reports ErrSessionBusy instead of queueing
	}
	if !call.FromDurableQueue {
		mb.submitted = append(mb.submitted, call)
	}
	return false, 0 // caller queues and returns nil, exactly like today
}

// drainOrRelease implements design §3: called by the owner at the exact
// point today's code calls messageQueue.PopFront at the end of a turn. If
// anything is queued, it is returned and ownership stays with the caller
// (state remains mbOwned). Otherwise ownership is atomically released
// (state flips to mbIdle) in the SAME critical section as the emptiness
// check, closing the P0-3 lost-wakeup window.
//
// epoch must be the value submit() returned when granting the caller
// ownership (BLOCKER-2). If the mailbox's current epoch has since moved on
// — a different, later caller became owner, e.g. because THIS call is a
// stale duplicate release from Run's unconditional cleanup defer running
// after runTurn's own drain already ended the era — this is a safe no-op:
// it must not touch submitted/state/current, which belong to that later
// owner now.
func (mb *mailbox) drainOrRelease(epoch uint64) (SessionAgentCall, bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return SessionAgentCall{}, false
	}
	if len(mb.submitted) > 0 {
		next := mb.submitted[0]
		mb.submitted = mb.submitted[1:]
		// Same stale-cancel shape as drainOrReleaseFinal's/drainAfterCancel's
		// equivalent branches (rounds 11-13) — this function has no live
		// production caller today (its only wrapper, releaseSessionReservation,
		// has zero call sites), but its own doc explicitly reserves it for a
		// future caller with no OS lock in play, so it gets the same
		// defensive clear rather than being a silent fifth instance waiting
		// to be reintroduced.
		mb.current.cancel = nil
		return next, true // caller runs another turn; state stays mbOwned
	}
	// Nothing queued AT THE INSTANT OF THIS CHECK, and — because mu is
	// held — nothing CAN be queued between this check and the state flip
	// below.
	if mb.testDrainSeam != nil {
		mb.testDrainSeam()
	}
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return SessionAgentCall{}, false
}

// drainOrReleaseFinal implements round 11 review, HIGH-1: it replaces the
// former two-step "release to mbIdle under mb.mu, THEN — outside mb.mu, in
// drainOrReleaseMerged — separately check the legacy messageQueue and only
// THEN release the OS session lock much later, once Run's whole call stack
// unwinds" shape with one atomic operation, so that "mb.state == mbIdle"
// (observable in-process via IsSessionBusy/submit/CancelAll/IsBusy, all of
// which just read mb.state under mb.mu) can no longer become true while the
// OS-level session.SessionLock this same call is still holding is not yet
// released.
//
// Without this, a same-process caller that saw IsSessionBusy(sessionID) ==
// false and tried to become the new owner via submit() — legitimately, that
// IS what "not busy" is supposed to mean — could reach
// session.TryAcquireSessionLock and get a spurious SessionLockBusyError from
// its own process's prior turn, which hadn't finished unwinding
// (runTurn's deferred wg.Wait() for title generation, then Run's own
// deferred lk.Release()) yet. Because tryReserveSession's "someone already
// owns it" branch never re-queues (submit()'s owner-branch assumes the
// eventual real owner will drain it), that manifested as a silently
// dropped user message, not a retryable error.
//
// release is called ONLY when BOTH mb.submitted came back empty — i.e.
// exactly the branch that used to flip mb.state to mbIdle directly. As of
// #296/P1-C it is called WITHOUT mb.mu held (see the mbReleasing const's doc
// for the full state-machine rationale): mb.state is set to mbReleasing and
// mb.mu is released before invoking it, then mb.mu is reacquired to finalize
// once it returns. It must release the OS-level session lock (or be a no-op
// when a.dataDir == "" and no lock was ever acquired — see
// drainOrReleaseMerged's call site). Any error it returns is surfaced to the
// caller for logging; it never blocks the state flip to mbIdle, mirroring
// how Run's own `defer lk.Release()` today only logs a Release failure rather
// than treating it as fatal — a failed unlock must not leave the mailbox
// permanently non-mbIdle with nobody left to retry it. A panic escaping
// release() is recovered (in the same way) and turned into an error for the
// same reason: the mailbox must always reach a terminal state, never get
// stuck in mbReleasing.
//
// Priority of branch evaluation within this function is now explicitly
// "replacement -> submitted -> release" (round 14 review, P0-A
// fix), closing a race where a late interrupt whose cancel lost to
// Stream's normal completion could strand the replacement: mb.replacement
// was set by interruptAndReplace under mb.mu, but agent.Stream returned
// nil (not context.Canceled), so runTurn took the normal-completion path
// instead of the isCancelErr branch's drainAfterCancel. The replacement
// check here ensures the replacement is recovered without releasing the
// OS lock, preserving the same atomic state machine semantics
// drainAfterCancel already provides for the cancel path.
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
// session.SessionLock.Release — a metadata truncate/remove) OUTSIDE mb.mu:
// it used to run WHILE mb.mu was held, so disk I/O briefly blocked every
// other in-process reader of this mailbox (IsSessionBusy, IsBusy, CancelAll,
// Cancel, QueuedPrompts) for that session — an unbounded control-plane stall
// if the filesystem/AV/SMB share hung, since release() has no context and no
// timeout. mb.state == mbReleasing stands in for the mutex as far as HIGH-1's
// atomicity goes (see the mbReleasing const's doc for why every other
// mutator already does the right thing around it).
//
// CRITICAL INVARIANT, closing brief #297 (a same-process reviewer's finding
// against an earlier version of this fix): once this call has entered
// mbReleasing, it can NEVER return hasNext=true / state==mbOwned again, no
// matter what lands in mb.submitted/mb.replacement during the release()
// window. release() has, by the time the finalize step below runs, already
// been invoked — successfully, with an error, or via a recovered panic — and
// there is no "un-release" operation: the OS lock this call was holding is
// either genuinely gone or its fate is unknown, either way NOT something
// this call can hand to a turn loop as "still holding it for you". An
// earlier draft of this function re-armed mbOwned and returned hasNext=true
// for exactly this case, on the theory that the turn loop still on the stack
// in runTurn could just keep going — but the turn loop's authority to run
// AT ALL came from that now-released lock; running another turn under it is
// indistinguishable, to a second process racing to acquire the same lock,
// from two owners of one session. A submit()/interruptAndReplace() landing
// in the release() window still correctly queues rather than becoming owner
// (mbReleasing reads as busy — see the mbReleasing const's doc), but the
// finalize step's job is to DRAIN that queued work OUT and hand it to the
// caller as orphaned, not to resurrect ownership for it. See orphaned's own
// doc below and drainOrReleaseMerged's doc in agent.go for how the caller is
// required to run it (a fresh, independent Run()-equivalent acquisition,
// mirroring coordinator.startDetachedRun's existing P0-B contract) instead
// of continuing the current turn loop on a lock that is no longer held.
func (mb *mailbox) drainOrReleaseFinal(
	epoch uint64,
	release func() error,
) (call SessionAgentCall, hasNext bool, orphaned []SessionAgentCall, releaseErr error) {
	mb.mu.Lock()

	if mb.epoch != epoch {
		mb.mu.Unlock()
		return SessionAgentCall{}, false, nil, nil
	}
	// Hard-stopped (P0-C): end the era rather than continuing into another
	// turn. Fall through to the mbReleasing path below (rather than an early
	// return) so the OS lock is still released and the mailbox still lands
	// on mbIdle — a shutdown must not leave the session marked busy with the
	// lock held. Queued work is NOT returned as "hasNext=true" for another
	// turn loop (that would start a provider turn during shutdown), but it
	// IS handed out as orphaned in the finalize step so drainOrReleaseMerged
	// durably enqueues it via restartOrphaned — see the finalize step's doc.
	if !mb.stopped {
		// Seam for deterministic test interleaving: fires right after the epoch check,
		// before ANY queue (replacement, submitted) is consulted — this is the
		// earliest deterministic pause point. Existing tests that relied on the seam
		// firing before the legacy-queue check are unaffected because their scenarios
		// now queue into mb.submitted instead, so the flow still reaches the same
		// submitted/release branches after the seam.
		if mb.testDrainSeam != nil {
			mb.testDrainSeam()
		}
		// P0-A fix: a late interrupt whose cancel lost the race to Stream's normal
		// completion lands here. mb.replacement was recorded by interruptAndReplace
		// under mb.mu, but agent.Stream returned nil (not context.Canceled), so runTurn
		// took the normal-completion path instead of the isCancelErr branch's
		// drainAfterCancel. Without this check, the replacement is silently stranded:
		// the drain releases ownership and the OS lock, and Run's defer
		// can only no-op it under a new era. Priority "replacement ->
		// submitted -> release" is now ONE atomic state machine operation,
		// matching drainAfterCancel's existing ordering. mb.current.cancel = nil — same invariant as every other "keep running"
		// branch (see mailbox_invariant_test.go). State stays mbOwned, OS lock untouched,
		// epoch NOT bumped — same semantics as the existing keep-running branches.
		if mb.replacement != nil {
			next := *mb.replacement
			mb.replacement = nil
			mb.current.cancel = nil
			mb.mu.Unlock()
			return next, true, nil, nil
		}
		if len(mb.submitted) > 0 {
			next := mb.submitted[0]
			mb.submitted = mb.submitted[1:]
			// mb.current.cancel must be cleared here too (round 12 review,
			// finding A — the SAME MEDIUM-1 shape as the former checkLegacy
			// branch, just on the more commonly hit mb.submitted path): by
			// the time this branch runs, it still holds the JUST-FINISHED
			// turn's own genCtx cancel — already invoked once via runTurn's
			// unconditional `cancel()` call right before this drain — inert
			// but NOT nil, which defeats Cancel()/InterruptAndReplace()'s
			// current.cancel==nil fallback gate exactly like the original
			// defect, for the whole window until the next turn's own
			// beginGeneration (which can be as long as title generation
			// takes).
			// dispatcherCancel is left untouched here (unlike the release
			// branch below): it's already the live runCancel from submit()/the
			// loop's own re-arm, still valid for the caller's own remaining
			// lifetime — nothing to reset.
			mb.current.cancel = nil
			mb.mu.Unlock()
			return next, true, nil, nil // caller runs another turn; state stays mbOwned, lock stays held
		}
	}

	// RELEASING (#296/P1-C): reached when stopped was already latched at
	// entry, OR when replacement/submitted all came back empty —
	// exactly the condition that used to flip mb.state straight to mbIdle
	// while still holding mb.mu across release()'s disk I/O. mbReleasing is
	// published and mb.mu is dropped BEFORE calling release(): every other
	// mutator's existing gate treats mbReleasing as busy/no-live-generation
	// (see the mbReleasing const's doc for the full per-method rationale),
	// which is what keeps HIGH-1 intact without holding the mutex over I/O
	// with no context and no timeout.
	//
	// current.cancel and dispatcherCancel are cleared HERE, on entry to
	// mbReleasing, not left for the finalize step below: hardStop() and
	// Cancel()/interruptAndReplace() can all run while release() is in
	// flight (they only need mb.mu for an instant, never blocked by the
	// in-flight I/O — that is the entire point of this state), and their
	// documented mbReleasing behavior (a no-op interrupt, since there is no
	// live generation left to cancel) depends on finding both nil. current.id
	// is preserved (monotonic, see the generation field's doc) — only the
	// spent cancel funcs are cleared, mirroring every other "era transition"
	// branch above.
	mb.current.cancel = nil
	mb.dispatcherCancel = nil
	mb.state = mbReleasing
	mb.mu.Unlock()

	// release() runs with NO mailbox lock held. A panic is recovered into an
	// error so the finalize step below always runs — the mailbox must never
	// get stuck in mbReleasing, whether release() errors or panics.
	if release != nil {
		releaseErr = callReleaseRecoveringPanic(release)
	}

	mb.mu.Lock()
	defer mb.mu.Unlock()

	// FINALIZE. release() has returned (or panicked, now folded into
	// releaseErr). This step ALWAYS lands on mbIdle — see this function's own
	// doc for why #296/P1-C's hand-back-to-mbOwned shape (an earlier draft of
	// this fix) was wrong: release() has already run by the time we get here,
	// so there is no OS lock left to keep running a turn loop under.
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil

	// The finalize step deliberately treats a stopped and a non-stopped
	// mailbox identically: any work that raced into the mailbox (via
	// CancelAll's hardStop latching stopped mid-release, or via a plain
	// submit()/interruptAndReplace during the release() window) is drained
	// out below and handed to the caller as orphaned, NOT run as another
	// in-process turn. The shutdown semantics — hasNext stays false so a
	// shutdown never starts another provider turn — live entirely in the
	// ENTRY check above, which skips the live branches for a stopped
	// mailbox. drainOrReleaseMerged durably enqueues the orphaned work via
	// restartOrphaned — a DB row, not a provider turn ("restart" stopped
	// being a fresh provider turn in task #340) — exactly matching the
	// abandon path's behavior on a latched mailbox (task #641 / twelfth
	// review N-3). The shutdown admission gate (tryAdmitRunWg) refuses any
	// Run during shutdown, and App.Shutdown stops the pump before closing
	// the DB, so the durable row is safely processed later.
	//
	// Historical note: an explicit `if mb.stopped { ... }` branch used to
	// live here (before #646 it discarded the raced-in work; #646 changed
	// it to enqueue). Task #652 (thirteenth review P3-3) deleted it once
	// it became byte-identical to this fall-through.

	// Drain everything that raced into the mailbox during the release()
	// window (a submit() or a recovered interruptAndReplace observed
	// mbReleasing as "not idle" and queued rather than became owner — see
	// the mbReleasing const's doc) OUT of the mailbox and hand it to the
	// caller as orphaned, instead of resurrecting mbOwned for it.
	//
	// Order (replacement, then submitted FIFO) mirrors the priority the
	// live branches above already use, purely so the orphaned slice's order
	// matches what a live turn loop would have run first — orphaned calls
	// are each restarted independently (see this function's own doc and
	// drainOrReleaseMerged's), so the order has no atomicity consequence,
	// only a "if you can only run one right now, run this one first" one.
	if mb.replacement != nil {
		orphaned = append(orphaned, *mb.replacement)
		mb.replacement = nil
	}
	if len(mb.submitted) > 0 {
		orphaned = append(orphaned, mb.submitted...)
		mb.submitted = nil
	}

	// This is the ONLY point mbIdle becomes observable, strictly AFTER the
	// OS lock was released (or release() failed/panicked — releaseErr is
	// surfaced for logging, but a failed unlock must not leave the mailbox
	// permanently non-idle with nobody left to retry it, mirroring how a
	// failed Run() defer lk.Release() today only logs). This is HIGH-1: no
	// same-process observer could have acted on "session is free" before
	// this line runs, because every observer's read (IsSessionBusy, IsBusy,
	// submit, interruptAndReplace, Cancel) treated mbReleasing as busy for
	// the whole window between the unlock above and this relock — and
	// mbIdle now means exactly that: no in-process owner, AND the OS lock is
	// free. orphaned calls are not exempt: the caller must acquire a FRESH
	// OS lock (a fresh Run(), or the coordinator.startDetachedRun-equivalent
	// restart) for each of them, exactly as if a brand new caller had
	// submitted them against an idle mailbox — because that is, at this
	// instant, genuinely what they are.
	return SessionAgentCall{}, false, orphaned, releaseErr
}

// callReleaseRecoveringPanic invokes release and recovers any panic it
// raises, folding it into the returned error instead of letting it unwind
// through drainOrReleaseFinal. #296/P1-C moved release() (real disk I/O:
// Truncate/Seek/Sync/sidecar-unlink/unlock/Close) outside mb.mu specifically
// so a slow filesystem cannot stall the control plane; a panic escaping
// uncaught would be worse than the stall it replaces — it would leave
// mb.state stuck at mbReleasing forever (mb.mu is not even held at the panic
// site to recover under), permanently reporting the session busy with no
// path back to mbIdle. Isolated into its own function (rather than an
// inline func(){}() in the caller) so the defer/recover pair has a single,
// obviously-correct home.
func callReleaseRecoveringPanic(release func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mailbox release callback panicked: %v", r)
		}
	}()
	return release()
}

// abandonOwnership implements Run's cleanup-defer release path (round 9
// review, BLOCKER-2a). It is NOT the same operation as drainOrRelease:
// drainOrRelease is called by an owner's OWN live turn loop, which can
// choose to keep running ("found something queued, stay owned, run it as
// the next turn"). Run's defer calls this instead specifically because it
// has NO live turn loop left — it fires on every return from Run(),
// including early bail-outs (OS-lock acquisition failure) and any
// early-return inside runTurn that skipped runTurn's own final drain (an
// error path). In both cases there is nobody left to hand a "keep
// running" answer to, so — unlike drainOrRelease — this ALWAYS ends the
// era at mbIdle.
//
// Unlike the pre-#308 version, this method does NOT drain submitted or
// replacement out to the caller. Entries left in submitted survive in
// place for the next owner to drain via drainOrReleaseFinal, and any
// pending replacement is folded INTO submitted (so it becomes a normal
// queued follow-up rather than pre-empting the next owner's first turn).
// The former messageQueue re-queue that the caller used to perform is no
// longer needed: the mailbox's own submitted queue is now the single
// source of truth for queued work.
//
// epoch behaves exactly as in drainOrRelease: a mismatch means this era
// already ended (a concurrent submit became the new owner, or
// drainOrReleaseFinal already released it) — a safe no-op that touches
// nothing, since whatever is in submitted/replacement now belongs to
// that later owner.
func (mb *mailbox) abandonOwnership(epoch uint64) (hadWork bool) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Fold any pending replacement into submitted so the next owner
	// treats it as a normal queued follow-up, not as a pre-emption
	// target for reclaimReplacementOrKeep. A replacement left in
	// mb.replacement across an era boundary would silently jump the
	// queue of whatever the next owner's first turn happens to be.
	if mb.replacement != nil {
		mb.submitted = append(mb.submitted, *mb.replacement)
		mb.replacement = nil
	}
	hadWork = len(mb.submitted) > 0
	mb.state = mbIdle
	mb.current.cancel = nil // preserve id (monotonic, see the field doc) — only the cancel func is spent
	mb.dispatcherCancel = nil
	return hadWork
}

// beginRelease atomically transitions the mailbox to mbReleasing state
// for a manual compaction. This mirrors the first half of drainOrReleaseFinal's
// mbReleasing transition pattern, allowing manual compaction to signal
// "OS lock release is in progress" separately from "still actively streaming".
//
// P1-2 fix (2026-08-09): Manual compaction used to call lk.Release() directly
// without any mbReleasing visibility (mailbox stayed mbOwned during the release).
// This meant that a hung filesystem/AV/SMB during lk.Release() would leave the
// mailbox permanently mbOwned, indistinguishable from an actively streaming turn.
// With mbReleasing visibility, diagnostic tools and future code can distinguish
// "releasing OS lock" from "still working".
//
// Returns true on successful transition, false if epoch mismatch (era already ended)
// or state cannot transition (already mbReleasing, mbIdle, etc.).
//
// The caller must:
//  1. Call beginRelease(epoch) to get mbReleasing visibility.
//  2. Release mb.mu (done by this method).
//  3. Call the actual release callback (e.g., lk.Release()).
//  4. Call finishRelease(epoch) to transition to mbIdle.
//
// This ensures the same "mbReleasing without holding mb.mu over I/O" invariant
// that drainOrReleaseFinal maintains.
func (mb *mailbox) beginRelease(epoch uint64) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Can only transition from mbOwned to mbReleasing.
	// If already mbReleasing, mbIdle, or some other state, this is a no-op.
	if mb.state != mbOwned {
		return false
	}

	// Clear the cancel handles before entering mbReleasing, matching drainOrReleaseFinal's
	// pattern. This ensures Cancel()/interruptAndReplace() correctly treat mbReleasing
	// as "no live generation left to cancel".
	mb.current.cancel = nil
	mb.dispatcherCancel = nil
	mb.state = mbReleasing
	return true
}

// finishRelease completes the release transition, moving from mbReleasing to mbIdle.
// This must be called after the actual release callback (e.g., lk.Release()) completes.
// Returns true on successful transition to mbIdle, false if epoch mismatch or state
// was not mbReleasing (stale/racing call).
func (mb *mailbox) finishRelease(epoch uint64) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.epoch != epoch {
		return false
	}
	// Must be in mbReleasing state to complete the transition.
	if mb.state != mbReleasing {
		return false
	}
	mb.state = mbIdle
	return true
}
