// The operator-facing control surface: cancel and interrupt-and-replace,
// queueing and inject, busy/queue queries, the runWg admission gate, and
// CancelAll shutdown.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

func (a *sessionAgent) Cancel(sessionID string) {
	// Cancel only the in-flight generation (design §4): a bare interrupt
	// (Ctrl-C, sessions kill, cost/token cap) must NOT discard durable
	// queued user intent. Previously Cancel unconditionally cleared the
	// queue — a latent second bug riding along with P0-2 that silently
	// dropped anything a caller had queued moments earlier via QueueMessage
	// for an unrelated reason. ClearQueue remains the one intentional
	// "drop everything queued" operation. The mailbox (whose current.cancel
	// is populated by beginGeneration in Run's loop and runTurn, and by
	// beginCompact for synchronous compactions) is now the cancel target
	// instead of activeRequests.
	//
	// Falls back to dispatcherCancel when no generation is live yet: Run
	// claims the mailbox (submit stores runCancel as dispatcherCancel) and
	// only calls beginGeneration once it reaches its turn loop, so a Cancel
	// landing in between — while the inter-process OS lock is still being
	// acquired — would otherwise find current.cancel nil and silently
	// no-op. Before the mailbox migration tryReserveSession wrote runCancel
	// straight into activeRequests, so that window WAS covered; keeping the
	// fallback preserves it (the task #206 "Cancel must never find a dead
	// placeholder" invariant).
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	genCancel := mb.current.cancel
	if genCancel == nil {
		genCancel = mb.dispatcherCancel
	}
	mb.mu.Unlock()
	if genCancel != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		genCancel()
	}
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	// The single intentional "drop everything queued" operation (design
	// §4): clears the mailbox's submitted/replacement/injects atomically
	// under its lock. Cancel no longer touches any of these; only this
	// method does.
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	a.getMailbox(sessionID).clearAll()
}

func (a *sessionAgent) QueueMessage(call SessionAgentCall) {
	a.getMailbox(call.SessionID).queue(call)
}

// InterruptAndReplace is the coordinator's single entry point for "interrupt
// and replace" (design §4), replacing the QueueMessage+Cancel two-step that
// P0-2 made self-defeating: Cancel deterministically wiped the very message
// QueueMessage had just queued the line before. It atomically records call
// as the replacement the current owner must run next, and cancels ONLY the
// in-flight generation — leaving the dispatcher (Run's turn loop) alive to
// drain the replacement via drainAfterCancel. Returns true when a turn was
// actually interrupted; false when the session was idle (nothing to cancel —
// the caller should queue call for the next Run itself).
func (a *sessionAgent) InterruptAndReplace(sessionID string, call SessionAgentCall) bool {
	cancelFn, hadOwner := a.getMailbox(sessionID).interruptAndReplace(call)
	if !hadOwner {
		return false
	}
	if cancelFn != nil {
		cancelFn()
	}
	return true
}

// InjectMessage — see SessionAgent interface comment. Persists immediately
// (UI updates via the same pubsub path that handleSendMessage uses) and, if
// the session is currently running, atomically queues the persisted row into
// the mailbox's injects list (stamped with the current generation id) so the
// next PrepareStep splices it into prepared.Messages without duplicating the
// DB write. The atomic busy-check + inject (mailbox.injectIfBusy, design §5
// stage 2.4) replaces the old non-atomic IsSessionBusy + injectQueue.Append
// pair.
func (a *sessionAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	msg, err := a.createUserMessage(ctx, call)
	if err != nil {
		return message.Message{}, err
	}
	// Atomic busy-check + inject under one mb.mu hold (design §5, stage 2.4):
	// replaces the non-atomic IsSessionBusy + injectQueue.Append pair that
	// had a window between check and append where the owner could finish and
	// release to mbIdle. When the session is idle, the message lives only in
	// the DB and is picked up by the next Run's preamble naturally.
	a.getMailbox(call.SessionID).injectIfBusy(msg)
	return msg, nil
}

// tryAdmitRunWg atomically checks shuttingDown and registers one unit of
// work in runWg — see admitMu's doc for why the check and the Add must be
// one operation relative to CancelAll's own critical section (closes a
// real "Add concurrently with Wait" race, P1-1). Returns false (does NOT
// call Add) if shutdown has already begun.
func (a *sessionAgent) tryAdmitRunWg() bool {
	a.admitMu.Lock()
	defer a.admitMu.Unlock()
	if a.shuttingDown.Load() {
		return false
	}
	a.runWg.Add(1)
	return true
}

func (a *sessionAgent) CancelAll() (stillBusy bool) {
	// Refuse all FUTURE Run() calls before touching anything else (closing
	// review, blocker 1). This must come first and must live on the agent
	// rather than the mailboxes: the sweep below can only latch mailboxes
	// that already exist, and mailboxes are created lazily, so a Run for a
	// session id the sweep never saw would otherwise get a fresh mailbox
	// with stopped == false and run a full turn nothing will cancel.
	a.admitMu.Lock()
	a.shuttingDown.Store(true)
	a.admitMu.Unlock()

	// Latch EVERY mailbox closed FIRST, before cancelling anything (round
	// 14 review, P0-C). Ordering is what makes the shutdown terminal: a
	// turn cancelled below immediately runs its cancel-handling branch,
	// and that branch drains replacement/submitted and starts the NEXT
	// turn. With `stopped` already latched, every drain refuses instead —
	// so no new provider request can begin while we are trying to exit.
	//
	// This is also why it hard-stops rather than reusing Cancel(): Cancel
	// deliberately targets only the current generation, leaving the
	// dispatcher (runCancel, the whole-Run() context) alive precisely so a
	// turn loop survives to run a replacement. Shutdown needs the opposite,
	// so hardStop hands back the DISPATCHER cancel too and we fire it.
	//
	// Latching is unconditional rather than gated on the mailbox currently
	// being owned: a Run() sitting between turns, or one that reaches its
	// drain in the window between this sweep and process exit, must be
	// refused as well.
	for _, mb := range a.mailboxes.Seq2() {
		dispatcherCancel, genCancel := mb.hardStop()
		// Generation first, then dispatcher: the generation cancel is what
		// promptly unblocks an in-flight provider stream, and cancelling
		// the dispatcher afterwards tears down the Run() that owns it.
		// Both are invoked outside mb.mu — hardStop has already returned.
		if genCancel != nil {
			genCancel()
		}
		if dispatcherCancel != nil {
			dispatcherCancel()
		}
	}

	// Stop every pending cache keep-alive timer AND cut off any replay
	// already in flight (bounded only by cacheKeepAliveCallTimeout, 30s,
	// otherwise) — defense in depth on top of tryAdmitRunWg's gate inside
	// fireCacheKeepAlive itself, so no new replay call is even attempted
	// once shutdown begins. Both maps swept under ONE cacheKeepAliveMu hold,
	// matching cancelCacheKeepAlive's own reasoning: fireCacheKeepAlive
	// moves an entry from "pending" to "in-flight" atomically, so a single
	// hold here sees a consistent snapshot instead of two separate looks
	// that could straddle that transition.
	a.cacheKeepAliveMu.Lock()
	for sessionID, entry := range a.cacheKeepAlive.Seq2() {
		entry.timer.Stop()
		a.cacheKeepAlive.Del(sessionID)
	}
	for sessionID, cancel := range a.cacheKeepAliveInFlight.Seq2() {
		cancel()
		a.cacheKeepAliveInFlight.Del(sessionID)
	}
	a.cacheKeepAliveMu.Unlock()

	// Wait for all active Run() goroutines to finish. This provides a true
	// join primitive instead of the old IsBusy() polling, which could report
	// "not busy" before the actual Run() goroutines had unwound (defer
	// cleanup, final DB writes, etc.). Use a 5-second timeout to match the
	// old grace period.
	grace := 5 * time.Second
	if a.cancelAllGrace > 0 {
		grace = a.cancelAllGrace
	}
	waitDone := make(chan struct{})
	go func() {
		a.runWg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// All Run() goroutines have finished. Clean shutdown.
		return false
	case <-time.After(grace):
		// Grace period expired but some Run() goroutines are still running.
		// Return true to signal forced shutdown.
		return true
	}
}

// IsBusy reports whether ANY session this agent knows about currently has a
// live owner. Task #206-followup (round 9 review, BLOCKER-1): this used to
// read activeRequests directly, but releaseSessionReservation (mailbox.
// drainOrRelease) stopped clearing the plain-sessionID activeRequests entry
// once the mailbox migration (P0-3, task #282) landed — tryReserveSession/
// Run's loop still WRITE it every turn (tryReserveSession in
// agent_run.go, runTurn in agent_turn.go) via
// activeRequests.Set, so after the FIRST turn any session ever ran,
// activeRequests permanently holds a non-nil (already-fired, inert)
// cancelFunc for it. The old activeRequests-based IsBusy therefore returned
// true forever after any session's first turn completed, which meant
// CancelAll's 5-second drain loop (App.Shutdown, reached by every `rush
// run` via `defer a.Shutdown()`) always ran to its full timeout instead of
// returning immediately once genuinely idle. mailboxes.state is the
// post-migration source of truth for "does this session have a live
// owner" (see IsSessionBusy's doc) and is correctly reset to mbIdle on
// release, so it does not have this staleness problem.
//
// `mb.state != mbIdle` (NOT `mb.state == mbOwned`) is deliberate as of
// #296/P1-C: mbReleasing means drainOrReleaseFinal's release() — the OS
// session-lock teardown — is running with mb.mu NOT held, on some OTHER
// goroutine than this one. If IsBusy() treated mbReleasing as "not busy",
// CancelAll's 5-second drain loop could see every mailbox as idle and
// return WHILE that release() disk I/O (and the whole-process DB teardown
// App.Shutdown runs right after CancelAll returns) is still in flight —
// reopening the exact class of race HIGH-1 closed, just observed through
// the shutdown path instead of a same-process "become the new owner" path.
func (a *sessionAgent) IsBusy() bool {
	for _, mb := range a.mailboxes.Seq2() {
		mb.mu.Lock()
		busy := mb.state != mbIdle
		mb.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

// IsSessionBusy reports whether sessionID currently has an owner (design §2's
// mapping table: mailbox.state != mbIdle replaces the old
// activeRequests.Get(sessionID) busy check). This is now the ONLY source of
// truth for the main-session busy state: releaseSessionReservation (via
// mailbox.drainOrRelease) no longer touches activeRequests at all, so
// activeRequests entries for a plain sessionID key would otherwise never be
// cleared. activeRequests itself is untouched by this migration and remains
// the cancel-target lookup Cancel/CancelAll/the peak-hours abort path use —
// call-site migration happens incrementally, one piece at a time (see the
// design doc's §7 migration plan).
func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.state != mbIdle
}

// QueuedPrompts reports how many calls are waiting for sessionID's current
// owner to finish, in the mailbox's submitted queue. All queue paths
// (QueueMessage, submit during busy session, abandonOwnership survivors)
// now go through the mailbox's submitted queue as the single source of
// truth.
func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return len(mb.submitted)
}

// QueuedPromptsList is QueuedPrompts' list counterpart — see its doc.
func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	mb := a.getMailbox(sessionID)
	mb.mu.Lock()
	mailboxCalls := append([]SessionAgentCall(nil), mb.submitted...)
	mb.mu.Unlock()

	if len(mailboxCalls) == 0 {
		return nil
	}
	prompts := make([]string, 0, len(mailboxCalls))
	for _, call := range mailboxCalls {
		prompts = append(prompts, call.Prompt)
	}
	return prompts
}
