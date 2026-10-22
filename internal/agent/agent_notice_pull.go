// The driver's notice pull (DUR-3, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.3, step 3): the session's driver -- the turn owner,
// holding the mailbox and the session-lock -- moves pending batch completion
// notices and session_notices rows into history at two points: turn start
// (runTurn, before history loads) and each step boundary of a normal turn
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
// messages were newly inserted. A pull error never fails the caller's
// turn: the affected row simply stays pending and is retried on the next
// pull (this turn's own step boundary, or a later turn).
func (a *sessionAgent) pullPendingNotices(ctx context.Context, sessionID string) (pulled []message.Message) {
	if sessionID == "" {
		return nil
	}
	// Batch-only pull: batch completion notices plus pending session
	// notices, both through the node-backed facts; no legacy void queries
	// and no async_jobs job-notice pull (the old table is gone).
	facts := coordinatorBatchCutoverFacts(a.coord)
	// claims[i] is the async_jobs claim id of pulled[i] -- always "" now:
	// batch notices and session_notices rows carry no legacy claim identity.
	// The reaction chain guard's key (#1113) stays: chainHasClaim("") is
	// false, so the "" claims are safe.
	claims := make([]string, 0, 8)
	bp, err := facts.PullBatchNotices(ctx, a.messages, sessionID)
	if err != nil {
		slog.Warn("notice pull: batch notice pull failed", "session_id", sessionID, "err", err)
	}
	for _, p := range bp {
		pulled = append(pulled, p.Message)
		claims = append(claims, "")
	}
	sp, err := facts.PullPendingSessionNotices(ctx, a.messages, sessionID, buildSessionNoticeMessageParams)
	if err != nil {
		slog.Warn("notice pull: batch session notice pull failed", "session_id", sessionID, "err", err)
	}
	for _, p := range sp {
		pulled = append(pulled, p.Message)
		claims = append(claims, "")
	}
	// Doc sec.3.4: moving a notice into history is progress for the
	// supervision countdown -- except a supervision check-in itself, which
	// must not reset the backoff it just grew, and a completion of the
	// reaction chain guard's own idle launches (#1113): a supervision tick
	// that drains such a completion must not reset the backoff either, or
	// the no-progress pause after 6 ticks never arrives.
	coord := a.coord
	for i, m := range pulled {
		if m.NoticeKind == noticeKindSupervision {
			continue
		}
		if claims[i] != "" && coord != nil && coord.reactionChainHasClaim(sessionID, claims[i]) {
			continue
		}
		if coord != nil {
			coord.recordProgress(sessionID)
		}
		break
	}
	return pulled
}

// pullPendingNoticesForStep is PrepareStep's step-boundary pull (doc
// sec.3.3): the same pull, at a point where the turn already runs.
func (a *sessionAgent) pullPendingNoticesForStep(ctx context.Context, sessionID string) []message.Message {
	return a.pullPendingNotices(ctx, sessionID)
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

// buildBatchNoticeMessageParams converts a pulled batch_nodes selected
// notice row into the history message's params, mirroring
// buildJobNoticeMessageParams. Batch notices carry NO tool name (batch
// leaves resolve by payload, not by tool identity) and no timeout seconds:
// the row's own notice_kind/state/cause and the root's outcome summary are
// the single source of truth.
func buildBatchNoticeMessageParams(row session.BatchNoticeRow) message.CreateMessageParams {
	// H4 W2: a recovered node (State interrupted / OutcomeCode host_lost)
	// is an interruption, not a finished job and not a stop: FormatAsync
	// Completion checks Stopped BEFORE Interrupted (coordinator_background
	// .go), so a Cause=="stop" node recovered to interrupted/host_lost must
	// not keep Stopped. Cancelled is kept as-is: State "interrupted" never
	// equals "cancelled", so a recovered node can never read as Cancelled.
	recovered := row.State == "interrupted" || row.OutcomeCode == "host_lost"
	completion := AsyncCompletion{
		ToolCallID: row.ToolCallID,
		ToolName:   "",
		Content:    row.OutcomeSummary,
		IsError:    row.State == "failed" || row.OutcomeCode == "failed",
		TimedOut:   row.State == "timed_out",
		Stopped:    row.Cause == "stop" && !recovered,
		Cancelled:  row.Cause == "stop" && row.State == "cancelled",
	}
	// The structured state and the retained summary are kept; no cause or
	// executor envelope is invented.
	if row.Cause == "host_closing" || recovered {
		completion.Interrupted = true
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
		NoticeKind:          row.Kind,
		Origin:              origin,
	}
}
