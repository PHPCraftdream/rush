// wakeSession: the single wake primitive (phase-4 step 3, docs/plans/
// 2026-09-28-async-phase4-durable-core.md sec.3.4). It no longer persists
// anything itself -- by the time any caller reaches wakeSession, the fact is
// already durably committed: a non-sync async job's terminal transition
// already set its async_jobs row's delivery='pending' (work_ledger_
// transition.go), or the caller already inserted a session_notices row
// (agent_notice_pull.go's InsertSessionNotice, called from
// coordinator_background.go/work_ledger_timeout.go/supervision.go). This
// function's only remaining job is: if wake is true, submit a Drain call
// (agent_drain.go) to the session's owning driver -- idle becomes owner like
// any call, busy queues (mailbox_queue.go's mergeQueuedCall). The Drain's own
// turn-start pull is what actually moves the notice into history; wakeSession
// itself never builds notice text or a NoticeKind anymore.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// jobIdentity is the (owner, toolCallID) pair a completion's wake hint is
// keyed on. toolCallID is diagnostic-only now (logging) -- wake no longer
// needs an idempotency key of its own: a Drain call is regenerable (its
// turn-start pull re-derives whatever it would have reacted to), so there is
// nothing to deduplicate by job identity the way the old noticedBefore map
// did.
type jobIdentity struct {
	owner      string
	toolCallID string
}

// wakeSession submits sessionID's Drain call when wake is true. A no-op when
// wake is false (doc sec.3.4's wake-policy table: Stop/job_kill/a failed
// wake-up marker never wake). Errors are visible (ASYNC-09): a Drain
// failure after the fact was already committed cannot lose the fact -- it
// stays pending/pending-debt for the NEXT pull -- so this persists a durable
// wake-failed marker (session_notices, wake=0) instead of just logging.
func (c *coordinator) wakeSession(ctx context.Context, job jobIdentity, wake bool) (err error) {
	if !wake {
		return nil
	}
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

	call, buildErr := c.drainCallFor(ctx, job.owner)
	if buildErr != nil {
		return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
	}

	agent := c.agentFor(job.owner)
	admission := newTurnAdmission()
	_, runErr := agent.Run(withTurnAdmission(ctx, admission), call)
	if runErr != nil {
		// The task is already terminal and its fact already durably
		// committed -- this failure is about the DELIVERY turn, not the
		// job. A queued admission is reported via (nil, nil) by Run's own
		// contract, never as an error, so any non-nil error here is a real
		// failure to even queue/start (e.g. shutdown, session lock busy).
		slog.Warn("drain call failed after its underlying notice was committed",
			"session_id", job.owner, "job_id", job.toolCallID, "err", runErr)
		c.persistWakeFailedMarker(ctx, job, runErr)
		return runErr
	}
	return nil
}

// persistWakeFailedMarker persists a durable session_notices row (kind
// NoticeKindWakeFailed, wake=0 per doc sec.3.4's wake-policy table) recording
// that a Drain call failed after its underlying fact was already committed.
// The next successful pull surfaces it as an ordinary pending notice --
// visible exactly once, without forcing another immediate turn (wake=0).
func (c *coordinator) persistWakeFailedMarker(ctx context.Context, job jobIdentity, cause error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return
	}
	markerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	text := fmt.Sprintf(
		"Не удалось продолжить работу после события %s: %s. Событие сохранено; продолжение — при следующем ходе.",
		job.toolCallID, cause,
	)
	if err := c.asyncJobs.store.InsertSessionNotice(markerCtx, job.owner, session.NoticeKindWakeFailed, text, false, ""); err != nil {
		slog.Error("wakeSession: failed to persist wake-failed marker",
			"session_id", job.owner, "job_id", job.toolCallID, "err", err)
	}
}
