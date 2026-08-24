package agent

import (
	"context"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
)

// mailbox is the per-session owner/mailbox state machine described in
// docs/plans/2026-08-04-session-owner-mailbox-design.md. Stages 2.1-2.4
// wired tryReserveSession/releaseSessionReservation, InterruptAndReplace,
// the two-tier context split, and the inject path onto it; #268/P0-4 wired
// compaction onto beginCompact. #308 completed the migration: QueueMessage
// and all its drain consumers moved from the legacy messageQueue to the
// mailbox's own submitted queue, and messageQueue was removed entirely.
// activeRequests is retained for OnStepFinish abort-path cancel lookups
// — the sessionID+"-summarize" synthetic key it used to hold has been
// removed by #268. See the design doc for the full rationale; this file
// declares the mailbox type, its state constants and its fields only —
// the methods (submit, drainOrRelease, drainOrReleaseFinal,
// interruptAndReplace, drainAfterCancel, inject, drainInjects,
// beginGeneration, beginCompact, queue, popFirstSubmitted) live in
// mailbox_ownership.go, mailbox_interrupt.go, mailbox_inject.go,
// mailbox_generation.go and mailbox_queue.go, with no behavior deviation.
type mailboxState int

// mbReleasing is drainOrReleaseFinal's terminal state, entered ONLY on the
// "nothing queued anywhere" branch — the exact instant that used to flip
// straight to mbIdle while still holding mb.mu across the OS-lock release()
// call (#296/P1-C). It exists so that call can drop mb.mu BEFORE running
// release()'s disk I/O (Truncate/Seek/Sync/sidecar-unlink/unlock/Close, no
// context, no timeout) without opening the HIGH-1 gap that I/O-under-mu was
// added to close: a slow/hung filesystem, antivirus, or SMB share must not
// block submit/Cancel/InterruptAndReplace/IsSessionBusy/IsBusy/CancelAll for
// this session, but no same-process observer may ever be able to conclude
// "session is free" before the OS lock is genuinely gone.
//
// mbReleasing threads that needle by being neither mbIdle nor mbOwned, so
// every mutator's existing gate already does the right thing without a new
// per-method mbReleasing check:
//
//   - submit(): gated on `state == mbIdle` to become owner. mbReleasing
//     fails that check exactly like mbOwned does, so a submit() landing
//     during the release window queues into mb.submitted instead of
//     acquiring a lock that isn't free yet. drainOrReleaseFinal's finalize
//     step (below) re-checks mb.submitted/mb.replacement AFTER reacquiring
//     mb.mu — NOT to hand that work back to the current turn loop (rejected,
//     #297 review: release() has already run by then, so there is no OS
//     lock left to keep a turn loop going under — see drainOrReleaseFinal's
//     own doc), but to drain it out as `orphaned` for the caller to restart
//     independently, under its own fresh OS-lock acquisition.
//   - Cancel()/interruptAndReplace(): gated on `current.cancel`/
//     `state == mbOwned`. During mbReleasing, current.cancel is still nil
//     (cleared before the state left mbOwned) and state is not mbOwned, so
//     Cancel() falls back to dispatcherCancel (also nil by this point —
//     the call becomes a documented no-op: there is no generation left to
//     interrupt, the turn loop is already past its provider work and only
//     doing lock teardown) and interruptAndReplace() takes its existing
//     "nobody running" branch, exactly as it does for mbIdle. This is
//     intentional: a turn in mbReleasing has no in-flight provider call
//     left to cancel, and the caller's fallback ("start a fresh Run()") is
//     already the correct behavior once the finalize step lands mbIdle a
//     moment later.
//   - IsSessionBusy() (`state != mbIdle`) and IsBusy() (see its own updated
//     comment): both must, and do, report BUSY during mbReleasing — this is
//     the crux of HIGH-1. If either treated mbReleasing as idle, a
//     same-process caller could act on "session is free" before release()
//     has actually let go of the OS lock.
//   - drainAfterCancel()/abandonOwnership(): neither is ever called while
//     state == mbReleasing in production — drainOrReleaseFinal is THE only
//     place that sets or clears this state, and it holds mb.mu for both the
//     transition in and the transition out, so no other mutator's call site
//     can observe mbReleasing mid-call. Documented here rather than special
//     -cased in either function: nothing needs to change in them.
//   - hardStop() (CancelAll/shutdown) landing while state == mbReleasing:
//     it only sets the one-way `stopped` latch and returns whatever
//     current.cancel/dispatcherCancel currently are (both nil during
//     mbReleasing, so the caller's cancel calls are no-ops — correctly:
//     there is nothing left running to interrupt). `stopped` is not read
//     again at finalize: since #652 deleted the finalize step's dead
//     stopped branch, the finalize step is latch-blind — stopped and
//     non-stopped mailboxes finalize identically, and anything that
//     landed in mb.submitted/mb.replacement during the release window is
//     drained out as `orphaned` (durably enqueued via restartOrphaned,
//     a session_run_queue row, not a fresh provider turn — "restart"
//     stopped being a provider turn in task #340), never discarded and
//     never handed back to the turn loop. The latch's only read in
//     drainOrReleaseFinal is the ENTRY check: when hardStop lands BEFORE
//     the drain is even called, that check skips the live branches for
//     the stopped mailbox, keeping hasNext false so a shutting-down
//     process never starts another provider turn; when it lands DURING
//     the release window, nothing stopped-specific is needed at all.
//     See the finalize step's own doc in mailbox_ownership.go.
const (
	mbIdle      mailboxState = iota // no owner, nothing queued
	mbOwned                         // a turn loop holds ownership
	mbReleasing                     // release() (OS-lock I/O) is running with mb.mu NOT held; see drainOrReleaseFinal (#296/P1-C)
)

// generation identifies one in-flight turn (or, once #268 lands, one
// compact) within a session's mailbox. id is monotonic per session,
// bumped every time beginGeneration is called; cancel cancels ONLY this
// generation's context, never the whole dispatcher (see design §4).
type generation struct {
	id     uint64
	cancel context.CancelFunc
}

// pendingInject is one message queued via mailbox.inject, stamped with the
// generation id that was current at submit time so drainInjects (design §5)
// can decide which turns are responsible for splicing it into
// prepared.Messages.
type pendingInject struct {
	msg        message.Message
	afterGenID uint64
}

// mailbox holds all per-session ownership/queueing state behind one mutex,
// ultimately replacing activeRequests, messageQueue, and the former
// sessionStartMu reservation gate for a single session id (injectQueue and
// sessionStartMu are already deleted). See design doc §1 for the full
// field-by-field rationale.
type mailbox struct {
	mu sync.Mutex // single critical section for ALL fields below

	state mailboxState

	// dispatcherCancel is the durable, call-scoped cancel func — spans the
	// whole Run() call (every turn + every preamble), analogous to today's
	// runCancel registered by tryReserveSession. Never the target of an
	// interrupt; used only by CancelAll/process shutdown.
	dispatcherCancel context.CancelFunc

	// current is the active generation's id+cancel, or the zero value when
	// state != mbOwned. Interrupt/Cancel target THIS, never dispatcherCancel.
	current generation

	// submitted holds pending SessionAgentCall values submitted while
	// owned — replaces messageQueue for the "queue a normal follow-up"
	// case. Kept as an unbounded slice (matching messageQueue's FIFO
	// contract today) rather than a single slot; see design §7 open
	// question 1.
	submitted []SessionAgentCall

	// replacement, when non-nil, is an interrupt-and-replace payload that
	// must be consumed by the NEXT generation the owner starts, and the
	// CURRENT generation must be cancelled to make room for it.
	replacement *SessionAgentCall

	// injects holds messages already persisted to the DB, waiting to be
	// spliced into prepared.Messages by the owner's PrepareStep.
	injects []pendingInject

	// compact holds at most one pending manual-compact request. Present
	// only so a compact submitted while owned is remembered until
	// drain-or-release; not exercised until #268 (design §6) — unused
	// placeholder field for this stage.
	compact *fantasy.ProviderOptions

	// testDrainSeam, when non-nil, is invoked by drainOrRelease AFTER it has
	// observed mb.submitted empty but BEFORE it flips state to mbIdle — i.e.
	// exactly inside the critical section that used to NOT exist as a single
	// atomic unit before this migration (see drainOrRelease's doc and design
	// §3). drainOrReleaseFinal (round 11 review, HIGH-1; round 14 review,
	// P0-A fix) calls it at the analogous point too — right after the epoch
	// check, before ANY queue (replacement, submitted, legacy) is consulted;
	// run_reclaim_cancel_test.go relies on this exact position to land a real
	// sa.Cancel call inside the reclaim window (that test's scenario has
	// mb.submitted empty and mb.replacement nil, so the flow still reaches
	// checkLegacy after the seam). It exists solely so a test can
	// deterministically land a concurrent submit() call (or, via
	// drainOrReleaseFinal, a concurrent Cancel/InterruptAndReplace call)
	// inside that instant: since mu is still held while testDrainSeam runs,
	// a concurrent caller needing mu blocks on mu.Lock() until testDrainSeam
	// returns, making the interleaving reproducible on every run instead of
	// relying on goroutine-scheduling luck. nil in all non-test construction
	// paths (the zero value of mailbox), so it changes no production behavior
	// — mirrors the existing onFire test-seam idiom already used by
	// stream_watchdog.go elsewhere in this package.
	testDrainSeam func()

	// testLoopRearmSeam, when non-nil, is invoked by Run's turn loop
	// (agent_run.go) immediately BEFORE each iteration's beginGeneration(turnCancel)
	// call — i.e. strictly after any end-of-turn drain (drainOrRelease/
	// drainOrReleaseFinal/drainAfterCancel) has already released mb.mu, and
	// strictly before the loop re-arms the mailbox's current generation for
	// the next turn. It exists so a test can deterministically land a
	// concurrent Cancel()/InterruptAndReplace() call inside that exact
	// window and observe its effect BEFORE the loop's own re-arm can
	// overwrite mb.current.cancel — closing the gap
	// TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2 (#289)
	// used to be unable to close deterministically: without this seam, the
	// loop's re-arm and a concurrently-blocked Cancel() call race for the
	// NEXT mb.mu acquisition after a reclaim releases it, so whether Cancel
	// observes the reclaim's fixed state (dispatcherCancel populated,
	// current.cancel nil) or loses the race to the re-arm (which writes its
	// own fresh, unrelated turnCancel into current.cancel first) depends on
	// goroutine-scheduling luck. Unlike testDrainSeam (which fires WHILE
	// holding mb.mu, so a concurrent mb.mu-needing caller blocks on the seam
	// itself), this seam fires OUTSIDE mb.mu — the loop has not yet called
	// beginGeneration when it fires — specifically so a test can let a
	// blocked Cancel() goroutine actually ACQUIRE mb.mu, run to completion,
	// and release it while the loop is parked here, before waving the loop
	// through to make its own beginGeneration call. nil in all non-test
	// construction paths (the zero value of mailbox), so it changes no
	// production behavior — mirrors testDrainSeam's existing idiom.
	testLoopRearmSeam func()

	// testPreAbandonSeam, when non-nil, is invoked by runSummarize (agent_compaction.go)
	// strictly AFTER the manual-compaction OS session lock has been released
	// and strictly BEFORE the mailbox is flipped to mbIdle via
	// abandonOwnership. It exists to let a test deterministically observe,
	// at that exact instant, that the two release the invariant Run() and
	// mbReleasing both uphold: an OS lock being free must never trail
	// mb.state still reporting idle to a same-process caller. Concretely: a
	// test parked here must see mb.IsSessionBusy()-equivalent state == true
	// (this compaction is still the owner) while TryAcquireSessionLock
	// already succeeds from "another process" (the OS lock is already
	// free) — proving the lock was released before, not after, idle became
	// visible. Fires outside mb.mu (abandonOwnership has not been called
	// yet), mirroring testLoopRearmSeam's own idiom. nil (a no-op) in every
	// production path.
	testPreAbandonSeam func()

	// testPreSnapshotConsumeSeam, when non-nil, is invoked by runSummarize
	// (agent_compaction.go) strictly AFTER the caller's SummarizeSnapshot has been
	// captured and strictly BEFORE it is consumed by runSummarizeBody. It
	// exists to let a test deterministically land a concurrent SetModels (or
	// any other shared-state mutation) exactly inside the window a pre-#341
	// regression would have re-read shared state in, without relying on a
	// wall-clock time.Sleep race that a fast in-memory path can simply
	// out-run. nil (a no-op) in every production path.
	testPreSnapshotConsumeSeam func()

	// epoch identifies the current OWNERSHIP ERA: bumped every time state
	// transitions mbIdle -> mbOwned (a NEW caller becomes owner), never on
	// a continuing turn within the same era (beginGeneration's turn-level
	// `current.id` is a different counter — see its own doc). Round 9
	// review, BLOCKER-2: without this, drainOrRelease had no way to tell
	// "am I still the current owner calling this for the first time" apart
	// from "a stale/duplicate release call from an era that has already
	// ended" — Run's cleanup defer calls drainOrRelease unconditionally on
	// every return path, including ones where runTurn's own internal drain
	// already released ownership (or where an EARLY error return skipped
	// it entirely, leaving the era still open). A caller now presents the
	// epoch IT was granted ownership under; drainOrRelease is a safe no-op
	// if that epoch no longer matches — either because the era already
	// ended and moved on (a concurrent submit became the new owner) or
	// because it never held ownership in the first place.
	epoch uint64

	// stopped is the one-way hard-stop latch set by hardStop (process
	// shutdown / CancelAll). Once set, every "keep running" branch of every
	// drain refuses to hand work back to a turn loop, so a cancelled turn
	// can no longer pull the NEXT queued call in and start a fresh provider
	// request while the process is trying to exit.
	//
	// Round 14 review, P0-C: CancelAll used to call the ordinary
	// Cancel(sessionID), which cancels only the CURRENT generation. The
	// cancel-error branch in runTurn then immediately drained
	// replacement/submitted and looped into the next turn — on a runCtx
	// that was never cancelled, since Cancel deliberately does not touch
	// dispatcherCancel. Shutdown therefore did the opposite of stopping:
	// it cancelled turn N and started turn N+1, and App.Shutdown's bounded
	// wait then tore the DB down underneath a still-running agent.
	//
	// Deliberately one-way (never cleared): a mailbox that has been
	// hard-stopped belongs to a process that is exiting. Mailboxes are
	// per-process, so there is nothing to reset it for.
	stopped bool
}
