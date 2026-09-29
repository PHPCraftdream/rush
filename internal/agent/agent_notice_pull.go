// The driver's notice pull (DUR-3, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.3, step 3): the session's driver -- the turn owner,
// holding the mailbox and the session-lock -- moves pending async_jobs/
// session_notices rows into history at two points: turn start (runTurn,
// before history loads) and each step boundary of a normal turn
// (PrepareStep, agent_turn_step.go). Compaction steps never call this.
package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// pullPendingNotices pulls every pending notice row for sessionID into
// history (one DB transaction per row, doc sec.3.3) and reports which
// messages were newly inserted plus whether any of them carries wake=1 --
// the Drain turn's interim decision for this step (doc sec.3.4's reaction
// debt replaces this in step 4). A pull error never fails the caller's
// turn: the affected row simply stays pending and is retried on the next
// pull (this turn's own step boundary, or a later turn).
func (a *sessionAgent) pullPendingNotices(ctx context.Context, sessionID string) (pulled []message.Message, anyWake bool) {
	if a.asyncJobs == nil || a.asyncJobs.store == nil || sessionID == "" {
		return nil, false
	}
	store := a.asyncJobs.store

	jobPulled, err := store.PullJobNotices(ctx, a.messages, sessionID, buildJobNoticeMessageParams)
	if err != nil {
		slog.Warn("notice pull: listing pending async job notices failed", "session_id", sessionID, "err", err)
	}
	for _, p := range jobPulled {
		pulled = append(pulled, p.Message)
		anyWake = anyWake || p.Wake
	}

	noticePulled, err := store.PullSessionNotices(ctx, a.messages, sessionID, buildSessionNoticeMessageParams)
	if err != nil {
		slog.Warn("notice pull: listing pending session notices failed", "session_id", sessionID, "err", err)
	}
	for _, p := range noticePulled {
		pulled = append(pulled, p.Message)
		anyWake = anyWake || p.Wake
	}
	// Doc sec.3.4: moving a notice into history is progress for the
	// supervision countdown -- except a supervision check-in itself, which
	// must not reset the backoff it just grew.
	for _, m := range pulled {
		if m.NoticeKind != noticeKindSupervision {
			a.asyncJobs.recordProgress(sessionID)
			break
		}
	}
	return pulled, anyWake
}

// pullPendingNoticesForStep is PrepareStep's step-boundary pull (doc
// sec.3.3): same underlying pull as pullPendingNotices, but callers at this
// point already decided whether this turn runs (that decision is made once,
// at turn start) -- so only the newly-inserted messages matter here, not
// the wake bit.
func (a *sessionAgent) pullPendingNoticesForStep(ctx context.Context, sessionID string) []message.Message {
	pulled, _ := a.pullPendingNotices(ctx, sessionID)
	return pulled
}

// buildJobNoticeMessageParams converts a pulled async_jobs row into the
// history message's params, reusing FormatAsyncCompletion -- the ONE
// formatter -- fed entirely from the row's own columns (tool_name/
// timeout_seconds, step 3) instead of a second source of truth.
func buildJobNoticeMessageParams(row session.JobNoticeRow) message.CreateMessageParams {
	completion := AsyncCompletion{
		ToolCallID:     row.ToolCallID,
		ToolName:       row.ToolName,
		Content:        row.ResultContent,
		IsError:        row.ResultIsError,
		TimedOut:       row.State == "timed_out",
		TimeoutSeconds: row.TimeoutSeconds,
		Stopped:        row.NoticeKind == "job_kill",
		Cancelled:      row.NoticeKind == "session_cancel",
		Interrupted:    row.NoticeKind == "interrupted",
	}
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: FormatAsyncCompletion(completion)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		NoticeKind:          row.NoticeKind,
		Origin:              origin,
	}
}

// buildSessionNoticeMessageParams converts a pulled session_notices row into
// the history message's params. The row's own text is already the final,
// formatted wording (built once, at insert time, by supervision.go/
// work_ledger_timeout.go/coordinator_background.go) -- no second formatter.
func buildSessionNoticeMessageParams(row session.SessionNoticeRow) message.CreateMessageParams {
	kind := row.Kind
	if kind == session.NoticeKindBGShellDone {
		// Preserve today's exact behavior for this one kind: BackgroundJobNotice
		// alone carried it before step 3, with no distinct message-level
		// NoticeKind. session_notices.kind still records "bg_shell_done" for
		// bookkeeping/display (`sessions why`), just not as the message's
		// own NoticeKind.
		kind = ""
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: row.Text}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		NoticeKind:          kind,
	}
}
