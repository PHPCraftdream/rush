// Session ownership lifecycle: mailbox reservation, drain-and-release, and
// abandonment (tryReserveSession and friends), orphaned-call recovery via the
// durable run queue, and the activity-notify / watchdog-bump context plumbing
// shared by the turn, ownership, and compaction paths.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/session"
)

// getMailbox returns the per-session mailbox for sessionID, creating it
// lazily on first touch. Mirrors activeRequests' "one map, lazily
// populated, entries live forever" lifetime — see mailbox.go and
// docs/plans/2026-08-04-session-owner-mailbox-design.md §1.
func (a *sessionAgent) getMailbox(sessionID string) *mailbox {
	return a.mailboxes.GetOrSet(sessionID, func() *mailbox { return &mailbox{} })
}

// tryReserveSession is now a thin wrapper around mailbox.submit (design §3,
// stage 2 step 1 of the migration — see
// docs/plans/2026-08-04-session-owner-mailbox-design.md §7). Historically
// this used sessionStartMu + activeRequests directly; that had a real
// P0-3 lost-wakeup defect at the OTHER end of the reservation's lifetime
// (see releaseSessionReservation's doc below) even though the reserve side
// itself was already atomic. Routing through the mailbox's own mutex keeps
// the reserve and release sides of the reservation on the same lock, which
// is what closes that window.
//
// call must be the REAL call Run() received, not a placeholder: when the
// mailbox is already owned, submit appends call verbatim to mb.submitted for
// the current owner's end-of-turn drain to pick up as the next turn's
// SessionAgentCall — a placeholder with an empty Prompt would silently
// replace the caller's actual prompt with an empty one once drained.
//
// When becomeOwner is true, epoch is this ownership era's id (round 9
// review, BLOCKER-2) — the caller MUST present it to every subsequent
// releaseSessionReservation/drainOrReleaseMerged/abandonOwnership call it
// makes for the lifetime of this Run() call, so a stale/duplicate release
// (Run's cleanup defer firing after the era already legitimately ended) can
// never be mistaken for "release MY still-current ownership" and clobber a
// different, later owner's state. See mailbox.epoch's doc for the full
// rationale.
func (a *sessionAgent) tryReserveSession(call SessionAgentCall, reserveCancel context.CancelFunc) (becomeOwner bool, epoch uint64) {
	return a.getMailbox(call.SessionID).submit(call, reserveCancel)
}

// ReserveExclusive atomically claims exclusive ownership of sessionID's
// mailbox WITHOUT starting a turn — the same atomic mbIdle->mbOwned
// check-and-set beginCompact already provides for manual /compact (#268/
// P0-4), reused here for any caller that needs to hold ownership across a
// span of serving-side work (task #614: handleRerunMessage must cancel the
// old turn, delete the message tail, and delete the target message, all
// while guaranteed no concurrent Send/Rerun can become owner and write a
// new streaming message into the middle of that deletion).
//
// This is a single atomic operation, not "check IsSessionBusy, then
// reserve": the check and the mbIdle->mbOwned transition happen inside one
// mailbox.mu critical section (mailbox.beginCompact), so there is no window
// between "observed idle" and "became owner" for a concurrent submit() to
// slip through. Returns ok=false (fail closed) if the session is already
// owned (a turn or compaction in progress) or the mailbox has been
// hard-stopped — callers MUST treat that as "cannot proceed", not silently
// retry with a weaker check.
//
// On success the caller holds the reservation until it calls EITHER
// ReleaseExclusive (to drop ownership without running anything — e.g. the
// caller hit an error partway through its held work and wants any mailbox
// work that raced in during the hold handed off safely) OR
// RunWithReservedOwnership (to hand ownership straight into a turn loop
// with no gap). Exactly one of the two must be called, exactly once, or the
// session is stuck reporting busy forever (mirrors every other
// beginCompact/tryReserveSession caller's contract in this file). If the
// caller's own held work fails partway through, ReleaseExclusive still
// safely hands off anything that raced into the mailbox during the hold —
// see its own doc.
//
// CORRECTED (findings review, task #614 defect 2): the returned cancel is now a
// REAL cancellation signal that the caller can observe. The function returns
// holdCtx (the derived context created via context.WithCancel(ctx)) as the
// first return value, and holdCancel (the CancelFunc for that context) as the
// third. Cancel(sessionID)/CancelAll landing during the hold now cancel
// holdCtx, so the holder can select on it and abort its held work when a
// cancellation arrives. Concretely: a Cancel(sessionID) that lands while
// handleRerunMessage is mid-deletion DOES interrupt that deletion loop — the
// handler checks holdCtx.Err() after each failing operation (or once before
// the deletes) and bails out if it's non-nil, replying with an error and
// releasing the reservation via its armed defer.
//
// When RunWithReservedOwnership takes over, it rebinds the mailbox FIRST
// (via mailbox.rebindDispatcher, same epoch) to a fresh CancelFunc scoped
// to the turn loop's own runCtx, which IS wired to something meaningful
// (the turn's actual provider calls) — and only AFTER that rebind succeeds
// does it invoke this cancel, purely to release the now-superseded
// placeholder context (a context.WithCancel leak — go vet's lostcancel-
// style leak, not a functional signal). The rebind must win the epoch
// check before anything of the old era is torn down — see
// rebindDispatcher's own doc for why the swap is necessary and why it does
// not reopen any gap (rebindDispatcher never changes mb.epoch/state, only
// which CancelFunc is live for the still-continuous era).
func (a *sessionAgent) ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool) {
	holdCtx, holdCancel := context.WithCancel(ctx)
	mb := a.getMailbox(sessionID)
	epoch, ok = mb.beginCompact(holdCancel)
	if !ok {
		holdCancel()
		return nil, 0, nil, false
	}
	return holdCtx, epoch, holdCancel, true
}

// ReleaseExclusive drops a reservation taken by ReserveExclusive WITHOUT
// running a turn — used on every "cannot proceed" bail-out path so the
// session does not stay stuck reporting busy. Mirrors
// abandonOwnershipWithHandoff's contract (same function Run's cleanup defer
// uses): any work that raced into the mailbox's submitted/replacement
// queues during the hold (a concurrent submit() or InterruptAndReplace,
// both of which see mbOwned and queue rather than become owner) is handed
// to a fresh detached run rather than silently dropped.
func (a *sessionAgent) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	a.abandonOwnershipWithHandoff(sessionID, epoch)
}

// releaseSessionReservation clears the busy slot claimed by
// tryReserveSession, atomically checking for and returning any call queued
// in the meantime via mailbox.drainOrRelease (design §3). Before this, the
// old activeRequests.Del(sessionID) was strictly a release with no way to
// observe a concurrent submit that raced into the gap between the caller's
// own final "nothing queued" check and this release running — that gap is
// exactly the P0-3 lost-wakeup window. Any call handed back here (which can
// only happen when a concurrent submit lands between this release and the
// caller's own preceding drain check) is a genuine hasNext case; callers
// that cannot start another turn synchronously (e.g. Run's pre-loop bail-out
// paths) must not discard it silently — see those call sites for how it is
// handled.
//
// epoch must be the era id tryReserveSession (or a prior drainOrRelease
// call within the same era) returned. See mailbox.drainOrRelease's doc for
// what a mismatch means.
//
// Not used by runTurn's own end-of-turn drain any more (see
// drainOrReleaseMerged/drainOrReleaseFinal, round 11 review HIGH-1) since it
// releases nothing but the in-process reservation — callers that also need
// the OS-level session lock released atomically with the mbIdle flip must go
// through drainOrReleaseFinal instead. Kept as a thin wrapper for direct
// mailbox-level testing and any future caller with no OS lock in play.
func (a *sessionAgent) releaseSessionReservation(sessionID string, epoch uint64) (SessionAgentCall, bool) {
	return a.getMailbox(sessionID).drainOrRelease(epoch)
}

// drainOrReleaseMerged is runTurn's end-of-turn drain, folding in the OS-level
// session lock release (round 11 review, HIGH-1).
//
// Before this, the mailbox flipped to mbIdle — making IsSessionBusy/
// IsBusy/submit(), all same-process, in-memory reads under mb.mu, observe
// "not busy" — strictly BEFORE lk (the OS-level inter-process session lock)
// was actually released: that only happened much later, once runTurn
// returned, its own deferred wg.Wait() (title generation, up to several
// seconds) finished, control returned to Run's loop, and Run itself decided
// to return, running ITS deferred lk.Release(). A same-process caller that
// saw "not busy" in that window and tried to become the new owner via
// submit() + TryAcquireSessionLock got a spurious SessionLockBusyError from
// its own prior turn — and since tryReserveSession's "someone already owns
// it" branch never re-queues on that error path, the message was silently
// dropped, not just delayed.
//
// lk is the SAME *session.SessionLock Run() acquired once for the whole
// call (nil when a.dataDir == ""), and runCancel is Run's own whole-call
// CancelFunc (design §4 / round 11 review MEDIUM-1): dispatcherCancel must
// keep pointing at something live (runCancel, exactly what tryReserveSession/
// submit() already stores there for the "no live generation yet" window)
// rather than being left nil until the reclaimed turn's own beginGeneration
// call — otherwise a Cancel()/InterruptAndReplace() landing in that narrow
// window would silently no-op (Cancel) or falsely report success while
// cancelling nothing (InterruptAndReplace).
//
// The whole operation is one mailbox.drainOrReleaseFinal call, with exactly
// FOUR possible outcomes (corrected here — #297 review: an earlier version
// of this doc claimed only three cases and asserted "lk is simply never
// released for [a same-era reclaim] handoff" as the invariant covering
// every hasNext==true return, which #296/P1-C's first draft violated by
// adding a hand-back that returned hasNext==true AFTER lk had already been
// released):
//  1. mb.replacement (checked first — matching drainAfterCancel's priority,
//     see mailbox_interrupt.go) or mb.submitted is non-empty at the
//     time of the call, BEFORE any release attempt: pop and return it;
//     state stays mbOwned; lk is NOT touched (still held for the reclaimed
//     turn). The caller's loop runs it as the next turn under the SAME lk.
//  2. Both empty: this is the end-of-turn handoff — proceed to case 3.
//  3. Both mb.submitted and mb.replacement are empty: release lk (if non-nil) — now OUTSIDE mb.mu (#296/
//     P1-C: mb.state == mbReleasing stands in for the mutex so a hung
//     filesystem cannot stall the control plane) — then flip mb.state to
//     mbIdle. No same-process observer can see "not busy" until the OS lock
//     genuinely is gone (mbReleasing reads as busy throughout), closing the
//     HIGH-1 window described above.
//  4. Work (a submit()/interruptAndReplace()) races into mb.submitted/
//     mb.replacement WHILE case 3's release() is running, OR a hardStop
//     landed during that window. This is the ONLY case where
//     drainOrReleaseFinal itself CANNOT decide "keep this turn loop running"
//     — by the time it notices, release() has ALREADY run and lk is gone
//     (or its fate unknown, on error/panic), OR hardStop has latched and
//     the process is shutting down. It therefore drains that work out as
//     `orphaned` and — critically — still ends at mbIdle with
//     hasNext==false, exactly like case 3. For the hardStop case, this is
//     a durable enqueue (session_run_queue row) rather than starting a
//     provider turn — see drainOrReleaseFinal's finalize step doc. hasNext==true
//     now means, and only means, "lk is still held, keep the current turn
//     loop going on it" — cases 1 and 2 exclusively. `orphaned` is a
//     DIFFERENT signal: "this work has no lock and no turn loop; someone
//     must start a fresh one for it", handled below exactly like
//     coordinator.startDetachedRun's existing P0-B idle-session contract
//     (mailbox.interruptAndReplace's own doc: "the caller should instead
//     start a fresh Run() with call directly" — case 4 is that same
//     contract, reached via a different door). Never treat orphaned as a
//     queue the CURRENT lk can serve.
//
// lk is released only when no work is available to run next, avoiding the
// HIGH-1 window where "not busy" appears true while the OS lock is still
// held by a previous turn. This keeps the in-process and inter-process
// busy states in sync.
//
// ONLY safe to call from a live turn loop that will actually run the
// returned call as its next turn (runTurn's own end-of-turn drain). Run's
// cleanup defer, which has no turn loop left to hand a "yes, keep going"
// answer to, uses abandonOwnership instead (BLOCKER-2a) — calling THIS
// from there could reclaim the legacy queue into a brand new turn that
// then never runs, AND would release lk from the wrong place (Run's own
// deferred lk.Release() already owns that responsibility on that path).
func (a *sessionAgent) drainOrReleaseMerged(sessionID string, epoch uint64, lk *session.SessionLock, runCancel context.CancelFunc) (SessionAgentCall, bool) {
	var release func() error
	if lk != nil {
		release = lk.Release
	}
	next, hasNext, orphaned, releaseErr := a.getMailbox(sessionID).drainOrReleaseFinal(epoch, release)
	if releaseErr != nil {
		slog.Debug("agent.drainOrReleaseMerged: release session lock failed", "session_id", sessionID, "err", releaseErr)
	}
	// Case 4 (see doc above): work raced in during release() and cannot be
	// handed to the current (now lock-less) turn loop. Each orphaned call
	// gets its own independent, detached restart — the SAME mechanism
	// coordinator.startDetachedRun already uses for InterruptAndSend/
	// idle-session interrupt case, since that is
	// exactly what this now is: from orphaned's perspective the session is
	// idle (mbIdle, no lk held) the instant drainOrReleaseFinal returned.
	if err := a.restartOrphaned(orphaned); err != nil {
		slog.Error("agent.Run: failed to restart orphaned calls during ownership handoff",
			"session_id", sessionID,
			"num_calls", len(orphaned),
			"err", err)
	}
	if !hasNext {
		return SessionAgentCall{}, false
	}
	return next, true
}

// abandonOwnershipWithHandoff releases mailbox ownership and starts detached
// runs for any work left in the mailbox. This is the correct finalizer for
// error paths that exit ownership without a live turn loop, ensuring that
// queued calls are not stranded without a runner.
//
// P2-1 fix: also drains summarizeQueue when the session becomes idle, ensuring
// that pending summarise requests execute even when ownership transitions via
// non-web paths (CLI, detached runs, etc.). Previously, only the web handler's
// tail drained this queue, leaving summarise stranded if the session became idle
// through any other entry point.
//
// P2-5 fix: uses the atomic mb.abandonOwnershipAndPopSubmitted method instead
// of separately calling mb.abandonOwnership(epoch) followed by
// mb.popAllSubmitted(). The two-call sequence had a race window where a new
// owner could submit() (transitioning mbIdle -> mbOwned with a new epoch) and
// then queue() work, which popAllSubmitted would incorrectly hand off to
// restartOrphanedWithRetry instead of leaving it for the new owner's own drain.
// The atomic version closes this gap by checking the epoch and popping the
// submitted queue in a single critical section.
//
// It must be called with lk (if any) already released — the caller releases
// the OS lock first to avoid the HIGH-1 window where mbIdle becomes true
// while the lock is still held.
func (a *sessionAgent) abandonOwnershipWithHandoff(sessionID string, epoch uint64) {
	mb := a.getMailbox(sessionID)
	if popped := mb.abandonOwnershipAndPopSubmitted(epoch); popped != nil {
		slog.Error(
			"agent.Run: calls were pending when ownership had to be abandoned — starting detached runs to ensure they execute",
			"session_id", sessionID,
		)
		// Start detached runs for all work left in the mailbox.
		// abandonOwnershipAndPopSubmitted already folded replacement into
		// submitted, cleared current.cancel/dispatcherCancel, and left the
		// mailbox at mbIdle atomically, so the work we have here is exactly
		// what belonged to this era and nothing from a later one.
		//
		// P0-3 fix: check and surface the error from restartOrphanedWithRetry
		// instead of silently discarding it. The finalizer cannot return
		// the error to the original caller (Run's defer is already unwinding),
		// so we log it at ERROR level with full context to make failures
		// visible to operators.
		if err := a.restartOrphanedWithRetry(popped); err != nil {
			slog.Error("agent.Run: failed to restart orphaned calls during ownership handoff",
				"session_id", sessionID,
				"num_calls", len(popped),
				"err", err)
		}
	}

	// P2-1 fix: drain summarizeQueue after ownership is released and the session
	// is idle. This ensures that pending summarise requests execute even when the
	// session became idle through a non-web path (CLI, detached runs, etc.).
	// We run this in a detached goroutine with a fresh context.Background() to
	// avoid any dependencies on the current owner's request context.
	if a.summarizeQueue != nil {
		if opts, queued := a.TakeSummarizeQueue(sessionID); queued {
			go func() {
				if err := a.Summarize(context.Background(), sessionID, opts); err != nil {
					if errors.Is(err, ErrSummarizeQueued) {
						// Re-queuing due to ownership contention is expected and self-healing,
						// not an error. Log at debug level to avoid confusing operators.
						slog.Debug("agent: queued summarize re-queued due to ownership contention during ownership transition",
							"session_id", sessionID)
					} else {
						slog.Error("agent: queued summarize failed after ownership transition",
							"session_id", sessionID, "err", err)
					}
				}
			}()
		}
	}
}

// restartOrphaned starts one detached goroutine per entry in calls — the
// sessionAgent-level equivalent of coordinator.startDetachedRun (P0-B),
// needed here because drainOrReleaseMerged/drainOrReleaseFinal have no
// access to a *coordinator (agent and coordinator are separate layers;
// coordinator wraps sessionAgent, not the other way around).
//
// Task #340, ROUND 3 migration made this byte-identical to
// restartOrphanedWithRetry (both now durably enqueue via the run queue
// pump instead of the old direct-retry-loop design that originally
// distinguished them) — kept as a separate name for existing call sites
// and tests rather than a blanket rename, but implemented as a thin alias
// so the actual enqueue logic has exactly one copy. See
// restartOrphanedWithRetry's doc comment for the full design rationale.
//
// P0-3 fix: now propagates the error instead of silently discarding it.
func (a *sessionAgent) restartOrphaned(calls []SessionAgentCall) error {
	return a.restartOrphanedWithRetry(calls)
}

// restartOrphanedWithRetry durably enqueues calls for recovery by the run queue pump
// (task #340, ROUND 3 migration). This replaces the previous in-memory retry approach.
//
// Each call is enqueued to the durable session_run_queue table with an
// idempotency key derived from the session ID and the call's LogicalCallID
// (stable per logical request); a timestamp-based key is used only as a
// warned-about fallback when LogicalCallID is empty — see P2-1 below. The
// pump will retry the call until it
// succeeds or encounters a terminal failure (e.g., ErrCallAlreadyAttempted).
//
// P0-2 fix: made this function SYNCHRONOUS relative to its caller. It now waits for
// all durable enqueues to complete before returning, ensuring that no calls are lost
// between when the caller returns control and when the DB write commits. The caller
// (drainOrReleaseMerged, called from runTurn, called from Run's turn loop) is about to
// return control up its own call stack, possibly all the way out of Run() entirely.
// Durability via the run queue is critical here — the pump lives independently of any
// request/turn and will continue retrying after this function returns. By making the
// enqueue synchronous, we eliminate the data-loss window where a process crash after
// this function returns but before the goroutine's DB write commits would lose the call.
//
// This replaces the old retry loop (5 attempts with backoff) with a single durable
// enqueue operation. The pump's 3-second tick interval handles all retry timing,
// and its lease TTL (30 seconds) handles crash recovery.
//
// Error handling: if any enqueue fails, the call is written to the durable
// orphan outbox as a last-resort record the pump can recover (see the P0-3
// note at the call site — deliberately NOT an in-memory queue, which an
// idle session with no runner would never drain). The function returns an
// error if any enqueue fails, allowing the caller to log this as a critical
// failure. For marshal failures, the call is truly lost — there is no
// recovery path possible.
func (a *sessionAgent) restartOrphanedWithRetry(calls []SessionAgentCall) error {
	if len(calls) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	callErrs := make([]error, len(calls))

	for i, call := range calls {
		i, call := i, call
		wg.Add(1)
		go func() {
			defer wg.Done()

			// P0-1 fix: do NOT re-enqueue calls that originated from the durable queue.
			// These calls already have a durable row that will be retried by the pump.
			// Re-enqueueing would create a duplicate row (with the same idempotency key
			// if LogicalCallID is present, or a different key if LogicalCallID is lost),
			// leading to potential duplicate execution.
			//
			// When a pump processes a durable call and then fails (e.g., due to replacement
			// or interrupt), the pump will Nack the row back to pending state. The pump
			// (or another pump) will then re-lease and retry the SAME row. No new row
			// needs to be created.
			//
			// Calls with FromDurableQueue=false (e.g., direct web/CLI turns) need enqueue
			// because they don't have a durable row yet.
			if call.FromDurableQueue {
				slog.Debug("agent: skipping durable enqueue for FromDurableQueue call (already has a durable row)",
					"session_id", call.SessionID)
				return
			}

			// P2-1: Generate idempotency key from LogicalCallID (stable per logical request)
			// instead of timestamp (which changes on every retry). Fallback to timestamp
			// with warning if LogicalCallID is empty (should not happen in normal flow).
			var idempotencyKey string
			if call.LogicalCallID != "" {
				idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
			} else {
				slog.Warn("agent: LogicalCallID is empty, falling back to timestamp-based idempotency key (non-idempotent retries)",
					"session_id", call.SessionID)
				idempotencyKey = fmt.Sprintf("%s-%d", call.SessionID, time.Now().UnixNano())
			}

			// Convert to SessionAgentCallData for serialization
			callData := ToSessionAgentCallData(call)
			callDataJSON, err := json.Marshal(callData)
			if err != nil {
				slog.Error("agent: failed to serialize call data for durable enqueue",
					"session_id", call.SessionID, "err", err)
				callErrs[i] = fmt.Errorf("failed to serialize call data: %w", err)
				return
			}

			// Use a bounded context with timeout for the enqueue operation.
			// We don't inherit the caller's ctx because it may be cancelled (the
			// caller is about to return control up its own call stack). We use
			// context.Background() with a 30-second timeout to ensure the enqueue
			// doesn't hang forever.
			enqueueCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Durably enqueue the call BEFORE returning control (P0-2 requirement)
			// This ensures the call will eventually be executed even if this goroutine exits
			if enqueueErr := a.sessions.EnqueueRunQueueEntry(enqueueCtx, idempotencyKey, call.SessionID, callDataJSON); enqueueErr != nil {
				// Build safe log fields (SEC-1 fix: never log raw prompt)
				logFields := []any{
					"session_id", call.SessionID,
					"prompt_length", len(call.Prompt),
					"prompt_hash", promptHash(call.Prompt),
					"logical_call_id", call.LogicalCallID,
					"err", enqueueErr,
				}
				// Only add raw prompt in diagnostic mode (opt-in)
				if cliprovider.LogRawPromptEnabled() {
					logFields = append(logFields, "prompt", call.Prompt)
				}

				slog.Error("agent: failed to durably enqueue call for recovery", logFields...)

				// P0-3 fix: write to orphan outbox for durability. The outbox provides
				// a minimal durable record that can be recovered by the pump. This
				// is NOT an in-memory execution attempt — we only succeed if we can
				// durably persist the call somewhere.
				//
				// If even the outbox write fails, the call is truly lost and we
				// return an error to the caller (we do NOT mask data loss).
				outboxID := fmt.Sprintf("orphan-%s-%s", call.SessionID, idempotencyKey)
				// P2-2 fix: give the fallback write its OWN fresh 30s budget
				// instead of reusing enqueueCtx. Both tables live in the same
				// database, so a fresh context does not guarantee this write
				// succeeds where the primary one failed -- but reusing
				// enqueueCtx unconditionally guaranteed it could NOT: a
				// primary call that failed by exhausting its own 30s deadline
				// (a hung or locked DB, not a fast constraint/serialization
				// error) handed the fallback an already-expired context, so
				// the very failure class this outbox exists to catch got zero
				// attempt at the fallback. Rooted in context.Background(),
				// matching enqueueCtx's own rationale just above: the caller
				// is about to return control up its own call stack, and this
				// is the last-resort durability net for "we do NOT mask data
				// loss" (see the comment above) -- it must outlive the
				// caller's context the same way the primary enqueue attempt
				// already does, including surviving shutdown of the caller's
				// own request scope. It does NOT need to outlive the pump's
				// entire lifecycle the way run_queue_entry_exec.go's newDBCtx
				// does (see that file's doc for why its outcome writes are
				// even more detached) -- this is a one-shot write on a
				// goroutine that is about to exit either way.
				outboxCtx, outboxCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer outboxCancel()
				if outboxErr := a.sessions.WriteToOrphanOutbox(outboxCtx, outboxID, call.SessionID, callDataJSON); outboxErr != nil {
					logFields := []any{
						"session_id", call.SessionID,
						"prompt_length", len(call.Prompt),
						"prompt_hash", promptHash(call.Prompt),
						"logical_call_id", call.LogicalCallID,
						"outbox_id", outboxID,
						"enqueue_err", enqueueErr,
						"outbox_err", outboxErr,
					}
					if cliprovider.LogRawPromptEnabled() {
						logFields = append(logFields, "prompt", call.Prompt)
					}
					slog.Error("agent: failed to write orphan call to outbox (data loss)", logFields...)
					callErrs[i] = fmt.Errorf("failed to durably enqueue call and outbox write also failed: %w (enqueue error: %v)", outboxErr, enqueueErr)
					return
				}
				slog.Warn("agent: call written to orphan outbox for recovery",
					"session_id", call.SessionID,
					"prompt_length", len(call.Prompt),
					"prompt_hash", promptHash(call.Prompt),
					"logical_call_id", call.LogicalCallID,
					"outbox_id", outboxID,
					"enqueue_err", enqueueErr)
				// We still return an error because the primary path failed, but
				// the call is now durably persisted in the outbox and will be
				// recovered by the pump.
				callErrs[i] = fmt.Errorf("failed to durably enqueue call (written to orphan outbox for recovery): %w", enqueueErr)
				return
			}

			slog.Debug("agent: durably enqueued call for pump recovery",
				"session_id", call.SessionID, "idempotency_key", idempotencyKey)
		}()
	}

	// Wait for all enqueues to complete before returning
	wg.Wait()

	// Return first non-nil error, if any
	for _, err := range callErrs {
		if err != nil {
			return err
		}
	}
	return nil
}

// promptHash returns a safe hash of the prompt for logging (never the raw prompt).
// This prevents leaking user data (system prompts, history, secrets) into logs.
// Only used when RUSH_CLIPROVIDER_LOG_RAW_PROMPT is NOT enabled.
func promptHash(prompt string) string {
	if prompt == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(hash[:])[:16] // First 16 chars of hex hash (enough for collision resistance in logs)
}

// activityNotifyContextKey is the context key under which runTurn stores a
// composed "activity happened" callback (see withActivityNotify). It is
// unexported and local to this package: unlike tools.SessionIDContextKey,
// nothing outside package agent needs to read or set it directly — sub-agent
// delegation (the "agent" tool, agentic_fetch_tool) just forwards ctx
// unmodified, which is enough for the value to keep flowing.
type activityNotifyContextKey struct{}

// watchdogBumpContextKey is the context key for storing the stream watchdog's
// bump function. Used by runSummarizeBody and runSummarizeSilent to report
// LLM streaming progress to the watchdog so healthy long-running compaction
// doesn't get falsely killed as "no provider activity" (task #310).
type watchdogBumpContextKey struct{}

// withActivityNotify returns a child of ctx carrying a composed
// activity-notify callback: calling the returned callback records activity
// on lk (nil-receiver-safe, see SessionLock.RecordActivity) AND forwards to
// any ancestor notify callback already present on ctx.
//
// The "read the ancestor before overwriting" composition is what makes
// multi-level delegation chains (grandparent -> parent -> child) work: each
// level's runTurn calls withActivityNotify once on its own ctx before
// deriving genCtx from it, so a grandchild session's activity notify walks
// all the way back up through every ancestor session's lock, not just its
// immediate parent's. This is how a parent session's heartbeat stays alive
// purely from a delegated sub-agent's progress while the parent itself is
// blocked inside the "agent"/agentic_fetch tool call, producing no stream
// callbacks of its own.
func withActivityNotify(ctx context.Context, lk *session.SessionLock) context.Context {
	ancestor, _ := ctx.Value(activityNotifyContextKey{}).(func())
	self := func() {
		lk.RecordActivity() // nil-receiver-safe: no-op when lk is nil (a.dataDir == "")
		if ancestor != nil {
			ancestor()
		}
	}
	return context.WithValue(ctx, activityNotifyContextKey{}, self)
}

// notifyActivity invokes the composed activity-notify callback stored on ctx
// by withActivityNotify, if any. Safe to call on a ctx that never had
// withActivityNotify applied (e.g. in tests, or any ctx not descended from a
// runTurn call) — it is then a harmless no-op.
func notifyActivity(ctx context.Context) {
	if notify, ok := ctx.Value(activityNotifyContextKey{}).(func()); ok && notify != nil {
		notify()
	}
}

// withWatchdogBump stores the stream watchdog's bump function in ctx.
// Used by runSummarizeBody and runSummarizeSilent to report LLM streaming
// progress during compaction (task #310).
func withWatchdogBump(ctx context.Context, bump func()) context.Context {
	return context.WithValue(ctx, watchdogBumpContextKey{}, bump)
}

// notifyWatchdog invokes the watchdog bump function stored on ctx by
// withWatchdogBump, if any. Safe to call on a ctx that never had
// withWatchdogBump applied — it is then a harmless no-op.
func notifyWatchdog(ctx context.Context) {
	if bump, ok := ctx.Value(watchdogBumpContextKey{}).(func()); ok && bump != nil {
		bump()
	}
}
