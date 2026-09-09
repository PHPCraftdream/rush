package server

// Rerun-from-a-user-message: the atomic cancel/wait-idle/delete-tail/re-run sequence and its lost-prompt recovery. Split out of handlers_agent.go when the 1000-line file limit landed; it was more than half that file on its own.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
)

// handleRerunMessage is an atomic "retry from this user message": it cancels
// any in-flight agent run, waits for idle, deletes every message created AFTER
// the target user message, then deletes the target itself and re-runs the agent
// with the same prompt. Run() creates a fresh user message so the history reads
// naturally. All steps happen in one goroutine — no client-side race.
func handleRerunMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RerunMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	targetMsg, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "message not found")
		return
	}
	if targetMsg.Role != message.User {
		c.reply(msg.ID, EventError, nil, "can only rerun user messages")
		return
	}

	text := targetMsg.Content().Text
	if text == "" {
		c.reply(msg.ID, EventError, nil, "empty message")
		return
	}

	sessionID := targetMsg.SessionID
	slog.Info("ws: handleRerunMessage", "sessionID", sessionID, "messageID", p.MessageID,
		"contentPreview", text[:min(len(text), 80)])

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// Web sessions never prompt for permissions.
	autoApproveWebSession(a, sessionID)

	// 1. Cancel + clear queue if busy, then poll until idle (up to 10s). This
	// is a courtesy wait, NOT the safety mechanism: IsSessionBusy is a
	// snapshot, and a new Send/Rerun can legitimately start the instant after
	// this loop observes idle, before step 1a below claims exclusive
	// ownership. The wait exists purely so the common case (an old turn that
	// is already winding down) doesn't fail closed on step 1a just because
	// cancellation hasn't finished propagating yet.
	a.AgentCoordinator.Cancel(sessionID)
	a.AgentCoordinator.ClearQueue(sessionID)
	idle := false
	for i := 0; i < 100; i++ {
		if !a.AgentCoordinator.IsSessionBusy(sessionID) {
			idle = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// P1-6 fix: fail closed if the session is still busy after the timeout.
	// Provider/tool can legitimately respond to cancellation longer than 10s,
	// and the old owner may still be writing to history. Proceeding would race
	// between deletion and concurrent writes, corrupting the transcript.
	if !idle {
		slog.Warn("ws: rerun: session still stopping after timeout",
			"sessionID", sessionID,
			"messageID", p.MessageID,
			"timeout_seconds", 10)
		c.reply(msg.ID, EventError, nil, "session still stopping — please retry")
		return
	}

	// Test-only seam (task #614 regression test): fires strictly AFTER the
	// idle-poll above observed the session idle and strictly BEFORE
	// ReserveExclusive below claims ownership — i.e. exactly the window a
	// pre-fix rerun would have proceeded through unprotected. A test can pause
	// here to start a concurrent new Send/Rerun and prove it observes the
	// session busy (or fails its own reservation) instead of racing this
	// handler's tail-delete loop. nil (a no-op) in every production path,
	// mirroring the mailbox package's testDrainSeam/testLoopRearmSeam idiom.
	if rerunPostIdlePollSeam != nil {
		rerunPostIdlePollSeam()
	}

	// 1a. Task #614 (F5 of the 2026-08-20 readonly release review): claim
	// EXCLUSIVE ownership of the session's mailbox before touching history.
	// This is the actual safety mechanism, not the idle-poll above: it is a
	// single atomic mbIdle->mbOwned transition (ReserveExclusive ->
	// mailbox.beginCompact), so there is no window between "observed idle"
	// and "became owner" for a concurrent Send/Rerun/InterruptAndSend to
	// slip through and start writing a new streaming assistant message while
	// this handler deletes the tail below. If the session is busy — even if
	// it raced busy again in the instant after the idle-poll above returned
	// — ReserveExclusive fails and this handler fails closed: nothing is
	// deleted, and the operator is told to retry rather than risking a
	// force-delete of a message a live new turn is writing.
	//
	// Ownership is held from here through the tail delete, the target
	// delete, and the handoff into RunWithReservedOwnership below — it is
	// never released and re-claimed in between, which is what closes the
	// F5 hole: releasing after delete and re-reserving for the replacement
	// Run would reopen exactly the same "another caller can become owner in
	// the gap" window this reservation exists to close.
	holdCtx, epoch, reserveCancel, reserved := a.AgentCoordinator.ReserveExclusive(ctx, sessionID)
	if !reserved {
		slog.Warn("ws: rerun: could not claim exclusive ownership (session became busy again)",
			"sessionID", sessionID, "messageID", p.MessageID)
		c.reply(msg.ID, EventError, nil, "session became busy again — please retry")
		return
	}
	// releaseOnBailout is true on every return path below UNTIL step 6 hands
	// ownership to RunWithReservedOwnership, which takes over final release
	// itself (mirroring Run()'s own single-release contract) — see the
	// defer's own check just before step 6.
	releaseOnBailout := true
	defer func() {
		if releaseOnBailout {
			a.AgentCoordinator.ReleaseExclusive(sessionID, epoch, reserveCancel)
		}
	}()

	// 1b. Task #622 (F-1), redesigned in #631: the reservation above proves
	// no OTHER caller in THIS process can be writing to the session, but
	// says nothing about a `rush run --session S` executing in a DIFFERENT
	// process. The old byte-heuristic inspection is replaced by a
	// kernel-attested SHARED lock probe on the session's lock file,
	// HELD from here through the tail delete and the target delete below:
	// while the shared lock is held, no process — including this one — can
	// acquire the exclusive lock, so no external agent RUN (a lock-taking
	// writer, e.g. another `rush run --session S`) can start mid-delete.
	// Lock-free writers are NOT excluded by it: `rush sessions inject`
	// (cmd/sessions_inject.go) writes a user row from a separate process
	// without ever touching the lock — see the writer enumeration in the
	// baseline-capture comment below. It is released just before the
	// step-6 handoff, because the replacement turn's own Run() takes the
	// exclusive lock at its normal acquire point and a shared lock still
	// held here would conflict with our own acquire.
	// holdExternalSilenceProof's doc spells out the fail-closed rules
	// (contention, unresolvable data directory, probe error).
	probe, refuse, why := holdExternalSilenceProof(a, sessionID)
	if refuse {
		slog.Warn("ws: rerun: refusing: "+why,
			"sessionID", sessionID, "messageID", p.MessageID)
		c.reply(msg.ID, EventError, nil, why)
		return
	}
	// probeHeld guards the bailout paths between here and the step-6
	// handoff. probe is never nil here: the refuse branch above (the only
	// path holdExternalSilenceProof can return a nil probe on) already
	// returned before this defer is registered, and TryHoldSessionLockShared
	// opens with os.O_CREATE, so even an absent lock file yields a held,
	// non-nil probe on every path that reaches this point.
	// probe.Release's own nil-safety is defensive belt-and-suspenders, not
	// load-bearing for any path reachable from here today.
	probeHeld := true
	defer func() {
		if probeHeld {
			probe.Release()
		}
	}()

	// 2. Delete every message AFTER the target, keep the target.
	//
	// task #615: created_at is stored in whole SECONDS (see
	// internal/db/sql/messages.sql), so a message inserted just before the
	// target and one inserted just after can share the same created_at
	// value. Messages.List already returns the full session in a
	// deterministic (created_at ASC, rowid ASC) total order (same file,
	// ListMessagesBySession) — that rowid tiebreaker is exactly what a
	// timestamp-only comparison here would be missing. So instead of
	// re-deriving an order from timestamps (which cannot distinguish
	// same-second before/after), find the target's position in that
	// already-ordered list and delete only the slice strictly after it.
	allMsgs, listErr := a.Messages.List(holdCtx, sessionID)
	if listErr != nil {
		c.reply(msg.ID, EventError, nil, "failed to list messages")
		return
	}
	// Check if the hold was cancelled while listing messages
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}
	targetIdx := -1
	for i, m := range allMsgs {
		if m.ID == targetMsg.ID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		// Fail closed: without a confirmed position in the ordered list we
		// cannot safely tell "before" from "after", so delete nothing.
		slog.Warn("ws: rerun: target message not found in session list",
			"sessionID", sessionID, "messageID", targetMsg.ID)
		c.reply(msg.ID, EventError, nil, "target message not found in session")
		return
	}

	// Test-only seam (task #614 regression test, reverse direction): fires
	// strictly AFTER ReserveExclusive above claimed ownership and strictly
	// BEFORE the tail-delete loop below runs. A test can pause here to prove
	// that a concurrent Send/Rerun attempted WHILE this handler holds the
	// reservation observes the session busy and cannot create a new
	// streaming message — the direction of the F5 race that actually matters
	// (a new turn starting mid-deletion), as opposed to
	// rerunPostIdlePollSeam's "someone else grabs the reservation before we
	// do" direction. nil (a no-op) in every production path.
	if rerunHoldingReservationSeam != nil {
		rerunHoldingReservationSeam()
	}

	// Final phase-boundary check before mutating history: once the loop
	// below is entered it runs to completion (see deleteCtx), so a Cancel
	// must be honoured HERE — tail untouched — rather than mid-loop, where
	// honouring it would leave the tail half-deleted with no rerun in
	// exchange (task #630).
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// deleteCtx is the context the tail-deletion loop and step 3's target
	// delete run under. It is holdCtx with cancellation stripped
	// (context.WithoutCancel): a Cancel landing between two loop iterations
	// must not kill the remaining Delete/ForceDelete calls, or the session's
	// history is left partially truncated and the post-loop check replies
	// "cancelled" with nothing to show for the lost messages (task #630).
	// The invariant: the tail is either untouched (cancel honoured at a
	// phase boundary above) or fully deleted — never half. WithoutCancel
	// rather than context.Background() to keep holdCtx's values, matching
	// the agentCtx idiom already used further down in this handler.
	deleteCtx := context.WithoutCancel(holdCtx)

	for i, m := range allMsgs[targetIdx+1:] {
		// Test-only seam (task #630): see rerunTailDeleteSeam's declaration.
		if rerunTailDeleteSeam != nil {
			rerunTailDeleteSeam(i)
		}
		if delErr := a.Messages.Delete(deleteCtx, m.ID); delErr != nil {
			if errors.Is(delErr, message.ErrMessageStillStreaming) {
				// The message is still streaming, but THREE separate proofs
				// back the orphan claim: the session was cancelled and polled
				// to idle (step 1), this handler holds the exclusive
				// reservation (step 1a), and no OTHER process holds the
				// session's OS lock (step 1b). Only under all three can this
				// row truly never receive a terminal Finish — in-process idle
				// alone would not prove that across processes. Force-delete
				// it to avoid corrupting the transcript by including partial
				// text in LLM context forever.
				slog.Info("ws: rerun: orphaned streaming message, force-deleting",
					"id", m.ID, "err", delErr)
				if forceErr := a.Messages.ForceDelete(deleteCtx, m.ID); forceErr != nil {
					slog.Warn("ws: rerun: failed to force-delete orphaned streaming message",
						"id", m.ID, "err", forceErr)
				}
			} else {
				slog.Warn("ws: rerun: failed to delete tail message", "id", m.ID, "err", delErr)
			}
		}
	}

	// Check if the hold was cancelled during tail deletion. This is the
	// LAST cancellation point the handler honours (task #630): honouring
	// here leaves the state "tail fully deleted, target intact, no rerun"
	// — the operator's own prompt survives and a retry re-enters cleanly.
	// Every step after this mutates the target itself; once step 3 has
	// deleted it, only the replacement turn's Run() can recreate it, so
	// from here on a Cancel is NOT honoured in this handler. It is already
	// delivered to the mailbox (the holdCancel WAS the cancel target),
	// where the agent layer rebinds and applies it to the replacement turn
	// instead — the correct place for a mid-rerun Cancel to land.
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// Test-only seam (task #630 follow-up): see rerunPreTargetDeleteSeam's
	// declaration.
	if rerunPreTargetDeleteSeam != nil {
		rerunPreTargetDeleteSeam()
	}

	// 3. Delete the original user message — Run() will recreate it. Runs
	// under deleteCtx (cancellation stripped) and is the COMMIT POINT: if
	// this Delete succeeds and the handler returned early, the user's own
	// words would be gone with nothing recreating them, so nothing below
	// may return without first handing off into RunWithReservedOwnership.
	// A Cancel arriving during or after this delete is left to the agent
	// layer: RunWithReservedOwnership is invoked with agentCtx (derived
	// from the request ctx, not holdCtx) and continues the same ownership
	// era regardless of the hold's cancellation state.
	if delErr := a.Messages.Delete(deleteCtx, targetMsg.ID); delErr != nil {
		slog.Warn("ws: rerun: failed to delete original user message", "id", targetMsg.ID, "err", delErr)
	}

	// Capture the SET of message IDs the recreate watermark compares
	// against (task #655, fourteenth-review P2-1/P3-1; seeded from the
	// pre-delete listing by #658, fifteenth-review P3-1). This is not an
	// exclusivity claim: the reservation and probe above exclude other
	// agent RUNS, but handleDeleteMessage, handleDeleteMessages and
	// handleUpdateMessageContent (handlers_messages.go) — all three of
	// which can only delete or edit an existing row — and the two
	// production callers of coordinator.InjectMessage (the WS
	// handleInjectMessage and notifyBackgroundJobDone's Phase-3 branch,
	// coordinator_background.go, on a detached BackgroundShell.OnDone
	// goroutine that outlives its turn) — the only writers in THIS
	// process that can ADD a row, since agent Run reserves the session
	// before writing any row — mutate rows with no ownership check and
	// may interleave here. One adder is cross-process: `rush sessions
	// inject` (cmd/sessions_inject.go, doInject) writes a User row in a
	// separate process taking no lock at all, so the step-1b probe does
	// not exclude it either. The set does not need any of them excluded:
	// a concurrent writer only adds a row whose ID was never in the set
	// or removes one that was, and ID membership — unlike the index or
	// count #651 used — is invariant under deletions anywhere in the
	// list, which is exactly what in-turn compaction (deleting
	// summarised rows below the replacement turn's prompt) invalidates
	// positionally.
	//
	// The set is SEEDED from allMsgs — the pre-delete listing captured at
	// step 2, still in scope here — and the post-delete List's IDs are
	// unioned in when that call succeeds. Every row in allMsgs predates
	// the replacement run, so the seed alone is a valid (superset)
	// baseline, and seeding UNCONDITIONALLY means the set is never nil:
	// the helper's scan always runs instead of it creating blind. On the
	// normal path the union changes nothing — for any row still present
	// when the helper scans, membership in the union equals membership in
	// the post-delete set alone, because a row that predates the run and
	// still exists was necessarily in the post-delete listing too. The
	// seed's extra IDs are rows the deletes removed (a nonexistent ID
	// suppresses nothing) or, when a tail delete failed, pre-existing
	// rows that must not suppress the recreate anyway — exactly what set
	// membership already gives them. The target's own ID is in the seed
	// unconditionally, and the helper still admits it via its explicit
	// targetID disjunct.
	//
	// When the post-delete List fails (e.g. a transient DB error that
	// clears before the helper runs seconds later), the set falls back to
	// the pre-delete seed alone, so the scan still recognises the
	// replacement turn's own createUserMessage row (its ID is not in the
	// seed) instead of appending a duplicate next to it once the turn
	// later errors. The residual gap is a row created by a concurrent
	// writer BETWEEN the two listings: on this failed-List path it is in
	// neither set, so a same-text foreign row could suppress the
	// recreate — the same concurrent-writer tolerance the helper's doc
	// documents. That window is not two adjacent statements: it spans
	// the whole tail-delete loop plus step 3's target delete — one
	// Messages.Delete (itself a get + delete-if-terminal + publish
	// round trip) per tail row — so it is milliseconds for a short tail
	// and hundreds of milliseconds for a long one.
	baselineIDs := make(map[string]struct{}, len(allMsgs))
	for _, m := range allMsgs {
		baselineIDs[m.ID] = struct{}{}
	}
	if msgs, listErr := a.Messages.List(deleteCtx, sessionID); listErr == nil {
		for _, m := range msgs {
			baselineIDs[m.ID] = struct{}{}
		}
	} else {
		slog.Warn("ws: rerun: failed to list messages after target delete; baseline ID set falls back to the pre-delete listing",
			"sessionID", sessionID, "err", listErr)
	}

	// Task #645 (twelfth-review N-2): recreate the user prompt if it was
	// lost, on every exit path past the commit point above that did not
	// reach a returned run — including a PANIC anywhere between the delete
	// and a normal return: before the handoff (e.g. at rerunPreHandoffSeam
	// or in hub.Broadcast) and, since task #655 (fourteenth-review M-2), in
	// the window where onHandoff has fired but the replacement turn's
	// createUserMessage has not run yet (onHandoff fires before runOwned).
	// Both unwind with runReturned still false, keeping this defer armed.
	// Registered strictly AFTER the delete so it never fires for the early
	// returns above (before step 3 there is nothing to recreate). It runs
	// before the releaseOnBailout/probeHeld defers (LIFO), so on panic
	// unwind it still runs while the reservation is held. On ordinary
	// returns the agent layer has already released, so correctness does NOT
	// depend on exclusivity — it depends on the gating here plus the
	// baseline-ID+text scan in recreateRerunPromptIfLost, which no
	// unrelated concurrent writer can spoof.
	//
	// The gate, restated from #651: a successful run must never be
	// second-guessed — its transcript, including any in-turn compaction, is
	// the completed turn's own business — so runReturned flips to true the
	// moment RunWithReservedOwnership RETURNS, disarming this defer on the
	// success path exactly as #651's releaseOnBailout gate did, and an
	// error return disarms it via recreateHandled after the explicit call
	// in the err != nil branch below. releaseOnBailout alone could not
	// express the M-2 window: onHandoff has already cleared it there, so
	// #651's gate let the prompt be lost.
	recreateHandled := false
	runReturned := false
	defer func() {
		if (releaseOnBailout || !runReturned) && !recreateHandled {
			recreateHandled = true
			recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
		}
	}()

	// 4. Re-arm Phase 4 autonomy.
	a.AgentCoordinator.ResetAutoResumeCounter(sessionID)

	// 5. Resolve model overrides (same priority as handleSendMessage).
	var smartOverride, fastOverride *agent.ModelOverride
	// Sessions.Get is read-only, but run it under deleteCtx too: holdCtx
	// may already be cancelled past the commit point, and failing the Get
	// would silently drop the session's model overrides for no reason.
	if sess, sessErr := a.Sessions.Get(deleteCtx, sessionID); sessErr == nil {
		if sess.SmartModelID != "" {
			smartOverride = &agent.ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
		}
		if sess.FastModelID != "" {
			fastOverride = &agent.ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
		}
	}

	// 6. Run the agent with the same prompt, handing off the reservation
	// claimed in step 1a directly into the replacement turn — see
	// RunWithReservedOwnership's own doc for why this must CONTINUE the same
	// ownership era rather than release-then-reacquire. The onHandoff callback
	// transfers release responsibility: when it fires, the defer above is
	// disarmed and runOwned's defer takes over. Any panic before onHandoff fires
	// is still covered by the defer, which releases via ReleaseExclusive.

	// Release the shared-ownership probe BEFORE the step-6 handoff: the
	// replacement turn's Run() takes the EXCLUSIVE session lock at its
	// normal acquire point (agent_run.go, runOwned), and a shared lock
	// still held by this process would conflict with our own acquire and
	// bounce the rerun we just set up. Everything destructive is done —
	// the delete completed while provably owner-free — so releasing here
	// reopens only the benign bounce window between this release and the
	// acquire inside RunWithReservedOwnership (see holdExternalSilenceProof's
	// doc: no data can be destroyed by an owner arriving after the commit
	// point). Released before the rerunPreHandoffSeam below so the seam —
	// which represents the handoff instant — observes no probe held.
	probe.Release()
	probeHeld = false

	// Test-only seam (task #614 F-2): fires right before Broadcast, i.e. before
	// RunWithReservedOwnership is called. Used to test that panics before the
	// onHandoff callback still release the reservation. nil (a no-op) in every
	// production path.
	if rerunPreHandoffSeam != nil {
		rerunPreHandoffSeam()
	}

	agentCtx := agent.WithCallOrigin(context.WithoutCancel(ctx), message.OriginWeb)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: true})
	if smartOverride != nil || fastOverride != nil {
		_, err = a.AgentCoordinator.RunWithReservedOwnership(agentCtx, sessionID, text, epoch, reserveCancel, func() { releaseOnBailout = false }, smartOverride, fastOverride)
	} else {
		_, err = a.AgentCoordinator.RunWithReservedOwnership(agentCtx, sessionID, text, epoch, reserveCancel, func() { releaseOnBailout = false }, nil, nil)
	}

	// The replacement run has RETURNED (success or error): from here the
	// recreate defer is disarmed — on success the completed turn owns the
	// transcript, on error the explicit call below handles the prompt. Every
	// panic path (pre-handoff, the handoff→createUserMessage window, and
	// mid-run) still unwinds with runReturned false and stays covered.
	runReturned = true

	// P2-2 fix: broadcast the actual busy state derived from mailbox ownership,
	// not from this request handler's lifetime.
	if !a.AgentCoordinator.IsSessionBusy(sessionID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: false})
	}

	if err != nil {
		slog.Error("ws: rerun agent error", "err", err)
		// Task #638/#644: recreate the user prompt if it was lost — see the
		// defer registered after step 3's delete for the full rationale.
		// Run it explicitly here (BEFORE the error reply) so a watching
		// client still sees the recreated message's CreatedEvent broadcast
		// ahead of the error reply, matching the pre-#645 behaviour; the
		// defer no-ops afterwards via recreateHandled.
		// The flag is set AFTER the call (fourteenth-review M-5): if the
		// call itself panics (List/Create on a closed DB), recreateHandled
		// stays false and the defer below runs on the unwind — but it can
		// retry only on the error returns where onHandoff never fired
		// (RunWithReservedOwnership's pre-handoff failures), where
		// releaseOnBailout is still true and the defer's gate is open. On
		// THIS dominant error path onHandoff has already fired and
		// runReturned is true, so the gate is closed regardless: a panic
		// in the call loses the prompt either way (28f37afc behaved
		// identically here), and the ordering matters only for that
		// narrow pre-handoff path.
		recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
		recreateHandled = true
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// recreateRerunPromptIfLost restores a rerun target deleted at step 3 of
// handleRerunMessage when the replacement turn never recreated it (task
// #638/#644, extended to panic paths by #645, redesigned around message-ID
// membership by #655 after the fourteenth review showed both the count- and
// position-based watermarks break when rows are deleted below the prompt,
// and made never-nil by #658 seeding the capture set from the pre-delete
// listing).
// The check is ID-MEMBERSHIP+TEXTUAL: a row proves the prompt is present
// iff it is a User message whose Content().Text equals the captured prompt
// text AND either its ID was NOT in the baseline set captured around the
// target delete (so it was written after the baseline was captured —
// normally the replacement turn's createUserMessage row or an earlier
// explicit call's, though not ONLY those: a concurrent writer with no
// ownership check, e.g. handleInjectMessage, can land a same-text row in
// that window too) or its ID IS the original target's (step 3's delete
// failed and the operator's own row survived untouched — already present,
// nothing to restore). Membership
// never depends on order or count, so deletions elsewhere in the list —
// in-turn compaction deleting summarised rows below the prompt, a
// concurrent handleDeleteMessage — cannot shift or spoof the watermark the
// way #651's index window was shifted. Unrelated concurrent writers are
// still ignored (their text does not match), and an EARLIER identical
// prompt that survived the tail delete (#644's "continue" shape) still does
// not suppress the recreate: its ID is in the baseline set and it is not
// the target. Idempotent: once a qualifying row exists, this no-ops.
//
// baselineIDs is never nil from the capture site (task #658): it is
// seeded from the pre-delete listing and unioned with the post-delete
// one, so even a failed post-delete List leaves a valid superset
// baseline and the scan always runs. There is deliberately no "watermark
// unknown, create unconditionally" branch: it minted a spurious
// duplicate whenever a transient List failure cleared before a
// replacement run that had written its own prompt errored
// (fifteenth-review P3-1), and the seed removes the need to choose
// between creating blind and suppressing blind.
func recreateRerunPromptIfLost(ctx context.Context, a *appPkg.App, sessionID string, baselineIDs map[string]struct{}, targetID string, text string) {
	allMsgs, listErr := a.Messages.List(ctx, sessionID)
	if listErr != nil {
		slog.Error("Failed to list messages while checking if prompt needs recreation",
			"sessionID", sessionID, "err", listErr)
		return
	}

	// A qualifying row — User, matching text, and either new since the
	// baseline or the never-deleted target itself — means the prompt is
	// already present; return without creating.
	for _, m := range allMsgs {
		_, inBaseline := baselineIDs[m.ID]
		if m.Role == message.User && m.Content().Text == text && (!inBaseline || m.ID == targetID) {
			return
		}
	}

	_, createErr := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:   message.User,
		Origin: message.OriginWeb, // a re-run is web-initiated; the recreated prompt keeps the web origin
		Parts:  []message.ContentPart{message.TextContent{Text: text}},
	})
	if createErr != nil {
		slog.Error("Failed to recreate lost user prompt",
			"sessionID", sessionID, "err", createErr)
	}
}
