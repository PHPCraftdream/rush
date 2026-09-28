// wakeSession: the single fact-then-wake primitive (design doc §2/§3;
// docs/plans/2026-09-27-async-phase2-spec.md §1-2). Every completion source
// (notifyAsyncCompletion, notifyBackgroundJobDone, workLedger.handleTimeout's
// wake_only branch) calls this instead of building its own
// SessionAgentCall/InjectMessage/c.Run.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
)

// jobIdentity is the (owner, toolCallID) pair that survives a job's removal
// from the live ledger map: by the time any caller can reach wakeSession,
// deliverLocked has already deleted the live record from
// bySession[owner].jobs (work_ledger.go, unchanged) -- see workLedger's
// noticedBefore doc for why the idempotency key lives on a separate map
// instead of the (now-gone) *asyncJob.
type jobIdentity struct {
	owner      string
	toolCallID string
}

// wakeSession persists notice as a session message (idempotent by
// workLedger.noticedBefore, keyed on job) and, when wake is true, ALWAYS
// additionally attempts a turn over it via ExistingMessageID -- whether that
// turn runs immediately (idle mailbox) or gets queued behind a live one
// (busy mailbox) is decided entirely by the agent's own Run, not by a
// pre-check here (see the step-2 comment below for why a busy pre-check is
// wrong).
//
// wake=false reproduces today's Phase-3 InjectMessage behavior exactly
// (persist + merge-if-busy via InjectMessage's own atomic check, never force
// a turn).
//
// noticeKind, when non-empty, is persisted on the new message's NoticeKind
// field (§5.3bis) -- "" for every ordinary finish/fail/cancel notice,
// "timeout_wake_only"/"timeout_terminated" for the two timeout events §5.5
// introduces.
//
// The caller's ctx carries the entry-channel origin and notice flags
// (WithCallOrigin, agent_prompt.go's autoResumedCtxKey/
// backgroundJobNoticeCtxKey) exactly as it does for c.Run/buildCall today --
// wakeNoticeCall's non-driver branch (buildCall) reads them from ctx the
// same way; the driver branch stamps them explicitly since driver.callFor
// does not consult ctx.
func (c *coordinator) wakeSession(ctx context.Context, job jobIdentity, notice, noticeKind string, wake bool) (err error) {
	if c.asyncJobs == nil {
		return errors.New("wakeSession: async job ledger unavailable")
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wakeSession panic", "session_id", job.owner, "job_id", job.toolCallID,
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("wakeSession panic: %v", r)
		}
	}()

	agent := c.agentFor(job.owner)

	// Step 1 (§1.2): persist the fact, idempotent by job identity (§1.3). A
	// repeated call for a job already noticed (retry after a failed Run
	// below, or a racing second trigger) skips straight to step 2 with the
	// already-known message id -- never a second persisted row.
	msgID, alreadyNoticed := c.asyncJobs.noticeFor(job)
	if !alreadyNoticed {
		call, buildErr := c.wakeNoticeCall(ctx, job.owner, notice, noticeKind)
		if buildErr != nil {
			return fmt.Errorf("wakeSession: build notice call: %w", buildErr)
		}
		msg, injectErr := agent.InjectMessage(ctx, call)
		if injectErr != nil {
			return fmt.Errorf("wakeSession: persist notice: %w", injectErr)
		}
		msgID = msg.ID
		c.asyncJobs.recordNotice(job, msgID)
	}
	// Supervision's own notice must NOT reset the backoff it just grew --
	// every other notice (job/sub-agent finish, timeout, stop) is "progress"
	// and resets it (supervision.go's recordProgress). See recordProgress's
	// own doc for why the tick's resulting turn is excluded here instead of
	// relying on pushDeadlineOnTurnEnd to distinguish it.
	if noticeKind != noticeKindSupervision {
		c.asyncJobs.recordProgress(job.owner)
	}
	if !wake {
		return nil
	}

	// Step 2 (§1.2): always attempt the wake -- Run's OWN atomic
	// tryReserveSession/mailbox.submit is the authoritative busy/idle
	// decision, not a pre-check here. If idle, it executes directly. If
	// busy, it QUEUES this call (ExistingMessageID set, so no duplicate
	// persist) into mb.submitted, which is drained as the mailbox owner's
	// own NEXT turn once the current one ends -- a guarantee that a mere
	// injectIfBusy splice (step 1 above, when the session was busy at
	// persist time) does NOT provide by itself: a current generation whose
	// LAST PrepareStep has already run before the splice landed would never
	// drain it, silently stranding the notice. Skipping this call whenever
	// IsSessionBusy looked true would reproduce exactly that failure mode
	// (observed: a delegated child's own async-job completion never woke a
	// fresh turn, so its parent was told "finished" over the child's
	// pre-result text). A stray extra splice being ALSO visible to the
	// current generation, on top of the guaranteed later turn, is the same
	// benign double-exposure the pre-phase-2 code already accepted (its
	// notify path called Run unconditionally too).
	wakeCall, buildErr := c.wakeNoticeCall(ctx, job.owner, notice, noticeKind)
	if buildErr != nil {
		return fmt.Errorf("wakeSession: build wake call: %w", buildErr)
	}
	wakeCall.ExistingMessageID = msgID

	admission := newTurnAdmission()
	result, runErr := agent.Run(withTurnAdmission(ctx, admission), wakeCall)
	switch {
	case runErr != nil:
		// ASYNC-09 (§1.4): visible state, not a Debug line. The task is
		// already terminal and delivered -- this failure is about the
		// DELIVERY turn, not the job -- so there is nothing left to retry
		// automatically; the marker tells the owner (and, next turn, the
		// model) that a message is waiting.
		slog.Warn("wake failed after notice was persisted",
			"session_id", job.owner, "job_id", job.toolCallID,
			"notice_message_id", msgID, "err", runErr)
		c.persistWakeFailedMarker(ctx, job, msgID, runErr)
		return runErr
	case admission.wasQueued():
		// Expected, not a failure (§3.4): the notice is already durable and
		// will merge into the live/queued generation on its own.
		c.asyncJobs.consumeNotice(job)
		return nil
	default:
		_ = result
		c.asyncJobs.consumeNotice(job)
		return nil
	}
}

// wakeNoticeCall builds the SessionAgentCall wakeSession uses for BOTH the
// persist step and the wake step, so both see identical settings and a
// driver-owned child's restricted-run allowlist baseline is re-inherited
// (§6.2) on every call, not just the first.
func (c *coordinator) wakeNoticeCall(ctx context.Context, owner, notice, noticeKind string) (SessionAgentCall, error) {
	if driver, ok := c.subAgentDrivers.get(owner); ok {
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok && driver.parentSessionID != "" {
			// §6.2: bound to THIS driver's own generation, so
			// releaseDriverIfScopeClosed's later clear (or a stale one from
			// an already-superseded registration) can never remove a copy a
			// NEWER driver re-armed.
			mgr.InheritSessionRunAllowlistForGeneration(driver.parentSessionID, owner, driver.generation)
		}
		call := driver.callFor(notice)
		// driver.callFor copies a frozen template; unlike buildCall below it
		// does not read ctx, so the notice badges/origin are stamped here
		// explicitly to keep them identical to the non-driver path.
		call.AutoResumed = true
		call.BackgroundJobNotice = true
		call.Origin = CallOriginFrom(ctx)
		call.NoticeKind = noticeKind
		return call, nil
	}
	pinned, err := c.resolveSessionModels(ctx, owner)
	if err != nil {
		return SessionAgentCall{}, err
	}
	call, err := c.buildCall(ctx, owner, notice, pinned, nil)
	if err != nil {
		return SessionAgentCall{}, err
	}
	call.NoticeKind = noticeKind
	return call, nil
}

// persistWakeFailedMarker persists the orchestrator-decided (2026-09-28)
// visible marker for a failed wake: a second notice-tagged message in the
// SAME session, distinguishable by NoticeKind="wake_failed", saying the
// event was saved and will be picked up on the next turn. Detached from
// ctx's cancellation (ctx may already be why Run above failed) but keeps
// its request-scoped values (origin, notice flags) via WithoutCancel,
// mirroring notifyBackgroundJobDone's Phase-3 InjectMessage timeout pattern.
func (c *coordinator) persistWakeFailedMarker(ctx context.Context, job jobIdentity, noticeMessageID string, cause error) {
	markerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	text := fmt.Sprintf(
		"Не удалось продолжить работу после события %s: %s. Событие сохранено; продолжение — при следующем ходе.",
		job.toolCallID, cause,
	)
	call, err := c.wakeNoticeCall(markerCtx, job.owner, text, "wake_failed")
	if err != nil {
		slog.Error("wakeSession: failed to build wake-failed marker call",
			"session_id", job.owner, "job_id", job.toolCallID, "notice_message_id", noticeMessageID, "err", err)
		return
	}
	if _, err := c.agentFor(job.owner).InjectMessage(markerCtx, call); err != nil {
		slog.Error("wakeSession: failed to persist wake-failed marker",
			"session_id", job.owner, "job_id", job.toolCallID, "notice_message_id", noticeMessageID, "err", err)
	}
}
