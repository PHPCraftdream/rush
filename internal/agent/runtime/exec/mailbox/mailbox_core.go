package mailbox

import (
	"context"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// mailbox is the per-session owner/mailbox state machine described in
// docs/plans/2026-08-04-session-owner-mailbox-design.md. Stages 2.1-2.4
// wired tryReserveSession/releaseSessionReservation, InterruptAndReplace,
// the two-tier context split, and the Inject path onto it; #268/P0-4 wired
// compaction onto BeginCompact. #308 completed the migration: QueueMessage
// and all its drain consumers moved from the legacy messageQueue to the
// mailbox's own submitted Queue, and messageQueue was removed entirely.
// activeRequests is retained for OnStepFinish abort-path cancel lookups
// — the sessionID+"-summarize" synthetic key it used to hold has been
// removed by #268. See the design doc for the full rationale; this file
// declares the mailbox type, its state constants and its fields only —
// the methods (Submit, DrainOrRelease, DrainOrReleaseFinal,
// InterruptAndReplace, DrainAfterCancel, Inject, DrainInjects,
// BeginGeneration, BeginCompact, Queue, PopFirstSubmitted) live in
// mailbox_ownership.go, mailbox_interrupt.go, mailbox_inject.go,
// mailbox_generation.go and mailbox_queue.go, with no behavior deviation.
type MailboxState int

// MbReleasing is DrainOrReleaseFinal's terminal state, entered ONLY on the
// "nothing queued anywhere" branch — the exact instant that used to flip
// straight to MbIdle while still holding mb.Mu across the OS-lock release()
// call (#296/P1-C). It exists so that call can drop mb.Mu BEFORE running
// release()'s disk I/O (Truncate/Seek/Sync/sidecar-unlink/unlock/Close, no
// context, no timeout) without opening the HIGH-1 gap that I/O-under-mu was
// added to close: a slow/hung filesystem, antivirus, or SMB share must not
// block Submit/Cancel/InterruptAndReplace/IsSessionBusy/IsBusy/CancelAll for
// this session, but no same-process observer may ever be able to conclude
// "session is free" before the OS lock is genuinely gone.
//
// MbReleasing threads that needle by being neither MbIdle nor MbOwned, so
// every mutator's existing gate already does the right thing without a new
// per-method MbReleasing check:
//
//   - Submit(): gated on `state == MbIdle` to become owner. MbReleasing
//     fails that check exactly like MbOwned does, so a Submit() landing
//     during the release window queues into mb.Submitted instead of
//     acquiring a lock that isn't free yet. DrainOrReleaseFinal's finalize
//     step (below) re-checks mb.Submitted/mb.Replacement AFTER reacquiring
//     mb.Mu — NOT to hand that work back to the current turn loop (rejected,
//     #297 review: release() has already run by then, so there is no OS
//     lock left to keep a turn loop going under — see DrainOrReleaseFinal's
//     own doc), but to drain it out as `orphaned` for the caller to restart
//     independently, under its own fresh OS-lock acquisition.
//   - Cancel()/InterruptAndReplace(): gated on `current.cancel`/
//     `state == MbOwned`. During MbReleasing, current.cancel is still nil
//     (cleared before the state left MbOwned) and state is not MbOwned, so
//     Cancel() falls back to dispatcherCancel (also nil by this point —
//     the call becomes a documented no-op: there is no generation left to
//     interrupt, the turn loop is already past its provider work and only
//     doing lock teardown) and InterruptAndReplace() takes its existing
//     "nobody running" branch, exactly as it does for MbIdle. This is
//     intentional: a turn in MbReleasing has no in-flight provider call
//     left to cancel, and the caller's fallback ("start a fresh Run()") is
//     already the correct behavior once the finalize step lands MbIdle a
//     moment later.
//   - IsSessionBusy() (`state != MbIdle`) and IsBusy() (see its own updated
//     comment): both must, and do, report BUSY during MbReleasing — this is
//     the crux of HIGH-1. If either treated MbReleasing as idle, a
//     same-process caller could act on "session is free" before release()
//     has actually let go of the OS lock.
//   - DrainAfterCancel()/AbandonOwnership(): neither is ever called while
//     state == MbReleasing in production — DrainOrReleaseFinal is THE only
//     place that sets or clears this state, and it holds mb.Mu for both the
//     transition in and the transition out, so no other mutator's call site
//     can observe MbReleasing mid-call. Documented here rather than special
//     -cased in either function: nothing needs to change in them.
//   - HardStop() (CancelAll/shutdown) landing while state == MbReleasing:
//     it only sets the one-way `Stopped` latch and returns whatever
//     current.cancel/dispatcherCancel currently are (both nil during
//     MbReleasing, so the caller's cancel calls are no-ops — correctly:
//     there is nothing left running to interrupt). `Stopped` is not read
//     again at finalize: since #652 deleted the finalize step's dead
//     Stopped branch, the finalize step is latch-blind — Stopped and
//     non-Stopped mailboxes finalize identically, and anything that
//     landed in mb.Submitted/mb.Replacement during the release window is
//     drained out as `orphaned` (durably enqueued via restartOrphaned,
//     a session_run_queue row, not a fresh provider turn — "restart"
//     Stopped being a provider turn in task #340), never discarded and
//     never handed back to the turn loop. The latch's only read in
//     DrainOrReleaseFinal is the ENTRY check: when HardStop lands BEFORE
//     the drain is even called, that check skips the live branches for
//     the Stopped mailbox, keeping hasNext false so a shutting-down
//     process never starts another provider turn; when it lands DURING
//     the release window, nothing Stopped-specific is needed at all.
//     See the finalize step's own doc in mailbox_ownership.go.
//   - InjectIfBusy() (mailbox_inject.go) is the one exception to "no new
//     per-method check needed": it is gated on `state == MbOwned`
//     specifically, NOT the MbIdle-fails/else-busy shape every other
//     mutator above uses, because unlike them it isn't about OS-lock
//     availability — it's about whether a live generation loop will ever
//     call DrainInjects again, which MbReleasing already answers no to
//     just as definitively as MbIdle does.
const (
	MbIdle      MailboxState = iota // no owner, nothing queued
	MbOwned                         // a turn loop holds ownership
	MbReleasing                     // release() (OS-lock I/O) is running with mb.Mu NOT held; see DrainOrReleaseFinal (#296/P1-C)
)

// generation identifies one in-flight turn (or, once #268 lands, one
// Compact) within a session's mailbox. id is monotonic per session,
// bumped every time BeginGeneration is called; cancel cancels ONLY this
// generation's context, never the whole dispatcher (see design §4).
type Generation struct {
	ID     uint64
	Cancel context.CancelFunc
}

// pendingInject is one message queued via mailbox.Inject, stamped with the
// generation id that was current at Submit time so DrainInjects (design §5)
// can decide which turns are responsible for splicing it into
// prepared.Messages.
type PendingInject struct {
	Msg        message.Message
	AfterGenID uint64
}

// mailbox holds all per-session ownership/queueing state behind one mutex,
// ultimately replacing activeRequests, messageQueue, and the former
// sessionStartMu reservation gate for a single session id (injectQueue and
// sessionStartMu are already deleted). See design doc §1 for the full
// field-by-field rationale.
type Mailbox struct {
	Mu sync.Mutex // single critical section for ALL fields below

	State MailboxState

	// dispatcherCancel is the durable, call-scoped cancel func — spans the
	// whole Run() call (every turn + every preamble), analogous to today's
	// runCancel registered by tryReserveSession. Never the target of an
	// interrupt; used only by CancelAll/process shutdown.
	DispatcherCancel context.CancelFunc

	// current is the active generation's id+cancel, or the zero value when
	// state != MbOwned. Interrupt/Cancel target THIS, never dispatcherCancel.
	Current Generation

	// CurrentCall is the immutable call being executed by the active
	// generation. It is retained only for in-process control paths that must
	// preserve non-serializable call options, such as a DiskProvider interrupt.
	CurrentCall *agentcall.SessionAgentCall

	// submitted holds pending agentcall.SessionAgentCall values submitted while
	// owned — replaces messageQueue for the "Queue a normal follow-up"
	// case. Kept as an unbounded slice (matching messageQueue's FIFO
	// contract today) rather than a single slot; see design §7 open
	// question 1.
	Submitted []agentcall.SessionAgentCall

	// Replacement, when non-nil, is an interrupt-and-replace payload that
	// must be consumed by the NEXT generation the owner starts, and the
	// CURRENT generation must be cancelled to make room for it.
	Replacement *agentcall.SessionAgentCall

	// ArmQueuedBuild, when non-nil, arms the retained build ownership on a
	// call at the moment Submit() enqueues it into submitted (Package L
	// round 2). Set only by sessionAgent.getMailbox; nil for directly
	// constructed mailboxes (tests), which keeps them retention-free.
	ArmQueuedBuild func(*agentcall.SessionAgentCall)

	// Injects holds messages already persisted to the DB, waiting to be
	// spliced into prepared.Messages by the owner's PrepareStep.
	Injects []PendingInject

	// Compact holds at most one pending manual-Compact request. Present
	// only so a Compact submitted while owned is remembered until
	// drain-or-release; not exercised until #268 (design §6) — unused
	// placeholder field for this stage.
	Compact *fantasy.ProviderOptions

	// TestDrainSeam, when non-nil, is invoked by DrainOrRelease AFTER it has
	// observed mb.Submitted empty but BEFORE it flips state to MbIdle — i.e.
	// exactly inside the critical section that used to NOT exist as a single
	// atomic unit before this migration (see DrainOrRelease's doc and design
	// §3). DrainOrReleaseFinal (round 11 review, HIGH-1; round 14 review,
	// P0-A fix) calls it at the analogous point too — right after the Epoch
	// check, before ANY Queue (Replacement, submitted, legacy) is consulted;
	// run_reclaim_cancel_test.go relies on this exact position to land a real
	// sa.Cancel call inside the reclaim window (that test's scenario has
	// mb.Submitted empty and mb.Replacement nil, so the flow still reaches
	// checkLegacy after the seam). It exists solely so a test can
	// deterministically land a concurrent Submit() call (or, via
	// DrainOrReleaseFinal, a concurrent Cancel/InterruptAndReplace call)
	// inside that instant: since mu is still held while TestDrainSeam runs,
	// a concurrent caller needing mu blocks on mu.Lock() until TestDrainSeam
	// returns, making the interleaving reproducible on every run instead of
	// relying on goroutine-scheduling luck. nil in all non-test construction
	// paths (the zero value of mailbox), so it changes no production behavior
	// — mirrors the existing onFire test-seam idiom already used by
	// stream_watchdog.go elsewhere in this package.
	TestDrainSeam func()

	// TestLoopRearmSeam, when non-nil, is invoked by Run's turn loop
	// (agent_run.go) immediately BEFORE each iteration's BeginGeneration(turnCancel)
	// call — i.e. strictly after any end-of-turn drain (DrainOrRelease/
	// DrainOrReleaseFinal/DrainAfterCancel) has already released mb.Mu, and
	// strictly before the loop re-arms the mailbox's Current Generation for
	// the next turn. It exists so a test can deterministically land a
	// concurrent Cancel()/InterruptAndReplace() call inside that exact
	// window and observe its effect BEFORE the loop's own re-arm can
	// overwrite mb.Current.Cancel — closing the gap
	// TestRun_CancelDuringLegacyReclaimWindow_ActuallyCancelsTurn2 (#289)
	// used to be unable to close deterministically: without this seam, the
	// loop's re-arm and a concurrently-blocked Cancel() call race for the
	// NEXT mb.Mu acquisition after a reclaim releases it, so whether Cancel
	// observes the reclaim's fixed state (dispatcherCancel populated,
	// current.cancel nil) or loses the race to the re-arm (which writes its
	// own fresh, unrelated turnCancel into current.cancel first) depends on
	// goroutine-scheduling luck. Unlike TestDrainSeam (which fires WHILE
	// holding mb.Mu, so a concurrent mb.Mu-needing caller blocks on the seam
	// itself), this seam fires OUTSIDE mb.Mu — the loop has not yet called
	// BeginGeneration when it fires — specifically so a test can let a
	// blocked Cancel() goroutine actually ACQUIRE mb.Mu, run to completion,
	// and release it while the loop is parked here, before waving the loop
	// through to make its own BeginGeneration call. nil in all non-test
	// construction paths (the zero value of mailbox), so it changes no
	// production behavior — mirrors TestDrainSeam's existing idiom.
	TestLoopRearmSeam func()

	// TestPreAbandonSeam, when non-nil, is invoked by runSummarize (agent_compaction.go)
	// strictly AFTER the manual-compaction OS session lock has been released
	// and strictly BEFORE the mailbox is flipped to MbIdle via
	// AbandonOwnership. It exists to let a test deterministically observe,
	// at that exact instant, that the two release the invariant Run() and
	// MbReleasing both uphold: an OS lock being free must never trail
	// mb.State still reporting idle to a same-process caller. Concretely: a
	// test parked here must see mb.IsSessionBusy()-equivalent state == true
	// (this compaction is still the owner) while TryAcquireSessionLock
	// already succeeds from "another process" (the OS lock is already
	// free) — proving the lock was released before, not after, idle became
	// visible. Fires outside mb.Mu (AbandonOwnership has not been called
	// yet), mirroring TestLoopRearmSeam's own idiom. nil (a no-op) in every
	// production path.
	TestPreAbandonSeam func()

	// TestPreSnapshotConsumeSeam, when non-nil, is invoked by runSummarize
	// (agent_compaction.go) strictly AFTER the caller's SummarizeSnapshot has been
	// captured and strictly BEFORE it is consumed by runSummarizeBody. It
	// exists to let a test deterministically land a concurrent SetModels (or
	// any other shared-state mutation) exactly inside the window a pre-#341
	// regression would have re-read shared state in, without relying on a
	// wall-clock time.Sleep race that a fast in-memory path can simply
	// out-run. nil (a no-op) in every production path.
	TestPreSnapshotConsumeSeam func()

	// Epoch identifies the current OWNERSHIP ERA: bumped every time state
	// transitions MbIdle -> MbOwned (a NEW caller becomes owner), never on
	// a continuing turn within the same era (BeginGeneration's turn-level
	// `current.id` is a different counter — see its own doc). Round 9
	// review, BLOCKER-2: without this, DrainOrRelease had no way to tell
	// "am I still the current owner calling this for the first time" apart
	// from "a stale/duplicate release call from an era that has already
	// ended" — Run's cleanup defer calls DrainOrRelease unconditionally on
	// every return path, including ones where runTurn's own internal drain
	// already released ownership (or where an EARLY error return skipped
	// it entirely, leaving the era still open). A caller now presents the
	// Epoch IT was granted ownership under; DrainOrRelease is a safe no-op
	// if that Epoch no longer matches — either because the era already
	// ended and moved on (a concurrent Submit became the new owner) or
	// because it never held ownership in the first place.
	Epoch uint64

	// Stopped is the one-way hard-stop latch set by HardStop (process
	// shutdown / CancelAll). Once set, every "keep running" branch of every
	// drain refuses to hand work back to a turn loop, so a cancelled turn
	// can no longer pull the NEXT queued call in and start a fresh provider
	// request while the process is trying to exit.
	//
	// Round 14 review, P0-C: CancelAll used to call the ordinary
	// Cancel(sessionID), which cancels only the CURRENT generation. The
	// cancel-error branch in runTurn then immediately drained
	// Replacement/submitted and looped into the next turn — on a runCtx
	// that was never cancelled, since Cancel deliberately does not touch
	// dispatcherCancel. Shutdown therefore did the opposite of stopping:
	// it cancelled turn N and started turn N+1, and App.Shutdown's bounded
	// wait then tore the DB down underneath a still-running agent.
	//
	// Deliberately one-way (never cleared): a mailbox that has been
	// hard-Stopped belongs to a process that is exiting. Mailboxes are
	// per-process, so there is nothing to reset it for.
	Stopped bool
}

// Mailbox generation arming: BeginGeneration re-arms the current
// generation each turn; BeginCompact atomically claims ownership for a
// compaction. Pure code move from mailbox.go.

// ActiveCallToken identifies the exact published call and generation observed
// under mb.Mu. The call pointer is an identity fence in addition to the
// monotonic ownership and generation counters.
// BeginGeneration implements design §5: called by Run's loop before each
// turn (replacing today's activeRequests.Set(call.SessionID, cancel)
// re-arm). It bumps the per-session generation counter and records cancel
// as the new Current Generation's cancel func, returning the new
// generation id.
func (mb *Mailbox) BeginGeneration(cancel context.CancelFunc) (genID uint64) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	mb.Current.ID++
	mb.Current.Cancel = cancel
	return mb.Current.ID
}

func (mb *Mailbox) SetCurrentCall(call agentcall.SessionAgentCall) {
	mb.Mu.Lock()
	copy := call
	mb.CurrentCall = &copy
	mb.Mu.Unlock()
}

func (mb *Mailbox) ClearCurrentCall(Epoch uint64) {
	mb.Mu.Lock()
	if mb.Epoch == Epoch {
		mb.CurrentCall = nil
	}
	mb.Mu.Unlock()
}

func (mb *Mailbox) CurrentCallSnapshot() (agentcall.SessionAgentCall, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.CurrentCall == nil || mb.State != MbOwned {
		return agentcall.SessionAgentCall{}, false
	}
	return *mb.CurrentCall, true
}

// CurrentCallState distinguishes an idle mailbox from an owned mailbox whose
// active call has not been published yet. The latter is retryable for
// interrupt delivery: consuming a pending interrupt before its in-process
// dependencies are visible could force an unsafe durable rebuild.
func (mb *Mailbox) CurrentCallState() (agentcall.SessionAgentCall, bool, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.State != MbOwned {
		return agentcall.SessionAgentCall{}, false, true
	}
	if mb.CurrentCall == nil {
		return agentcall.SessionAgentCall{}, true, false
	}
	return *mb.CurrentCall, true, true
}

// CurrentCallStateWithToken snapshots the active call and its delivery fence
// atomically. A caller must use the returned token for any later conditional
// handoff; the copied call alone is not sufficient to identify the generation.
func (mb *Mailbox) CurrentCallStateWithToken() (agentcall.SessionAgentCall, ActiveCallToken, bool, bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()

	if mb.State != MbOwned {
		return agentcall.SessionAgentCall{}, ActiveCallToken{}, false, true
	}
	token := ActiveCallToken{Epoch: mb.Epoch, Generation: mb.Current.ID}
	if mb.CurrentCall == nil {
		return agentcall.SessionAgentCall{}, token, true, false
	}
	token.Call = mb.CurrentCall
	return *mb.CurrentCall, token, true, true
}

// BeginCompact atomically claims mailbox ownership for a compaction (manual
// /Compact or inline auto-summarize). It is the atomic check-and-reserve
// that replaces the old non-atomic IsSessionBusy + runSummarize pair
// (#268/P0-4, design §6): the idle check and the state flip happen under
// one mb.Mu hold, so no concurrent Run or second compaction can slip
// between them.
//
// Returns (Epoch, true) when the caller becomes the sole owner. The
// Compact's cancel is stored in BOTH current.cancel and dispatcherCancel so
// Cancel(sessionID), CancelAll, and InterruptAndReplace reach it through
// the SAME mb.Current.Cancel field as a turn's generation cancel — no
// synthetic "sessionID-summarize" string key. The caller must present
// Epoch to AbandonOwnership when the compaction finishes.
//
// Returns (0, false) when the session is already owned (a turn or another
// compaction is in progress) or Stopped. The caller queues (manual /Compact
// via summarizeQueue) or skips.
//
// BeginCompact is NOT in mailbox_invariant_test.go's stale-handle table:
// that table covers operations that END or CONTINUE an era (drain/release/
// abandon), whose postcondition is current.cancel == nil. BeginCompact
// STARTS an era with a FRESH cancel — its postcondition is the opposite
// (current.cancel != nil). Its postconditions are covered by a dedicated
// test in the regression test file added alongside this change.
func (mb *Mailbox) BeginCompact(cancel context.CancelFunc) (Epoch uint64, ok bool) {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.State != MbIdle || mb.Stopped {
		return 0, false
	}
	mb.State = MbOwned
	mb.Epoch++
	mb.DispatcherCancel = cancel
	mb.Current.ID++
	mb.Current.Cancel = cancel
	return mb.Epoch, true
}

// RebindDispatcher replaces the dispatcher-scoped CancelFunc for the
// CURRENT ownership era with a new one, without touching state/current.id/
// submitted/Replacement — i.e. it changes WHICH CancelFunc Cancel(sessionID)
// and CancelAll ultimately invoke for this era, without ending or
// restarting the era itself.
//
// This exists for task #614's RunWithReservedOwnership: ReserveExclusive's
// caller (handleRerunMessage) holds the reservation across a span of
// serving-side work using a short-lived placeholder CancelFunc, then hands
// ownership to a turn loop that needs ITS OWN cancelable runCtx — the turn
// loop's Cancel/genCtx plumbing (agent_run.go's runOwned) must be able to
// tear down that specific context tree, not the placeholder's now-mostly-
// irrelevant one. Rebinding here (rather than dropping the reservation and
// re-claiming a fresh one with the real runCancel from the start) is what
// keeps the era — and therefore the atomic "no idle window" guarantee
// ReserveExclusive exists for — continuous: mb.Epoch, mb.State, and
// mb.Current.ID are all untouched, only the cancel func changes.
//
// Epoch must be the value ReserveExclusive/BeginCompact returned. Returns
// false (a safe no-op) if the era has since moved on — the caller no longer
// holds the reservation it thinks it does, so it must not treat its
// runCancel as wired into the mailbox and should fail closed instead of
// proceeding as if the rebind succeeded.
//
// Also returns false when mb.Stopped is latched — rebinding onto a
// hard-Stopped mailbox would wire a fresh dispatcher CancelFunc into a
// process that is exiting, which is unsafe. BeginCompact already refuses
// Stopped, this matches it.
func (mb *Mailbox) RebindDispatcher(Epoch uint64, cancel context.CancelFunc) bool {
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	if mb.Epoch != Epoch || mb.State != MbOwned || mb.Stopped {
		return false
	}
	mb.DispatcherCancel = cancel
	mb.Current.Cancel = cancel
	return true
}
