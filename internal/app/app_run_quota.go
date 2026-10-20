package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type cliQuotaStop struct {
	cause              error
	reset              time.Time
	terminal           map[string]bool
	snapshotIncomplete bool
}

// FinishedWork preserves durable results without consuming their pending notices.
type FinishedWork struct {
	SessionID string `json:"session_id"`
	JobID     string `json:"job_id"`
	State     string `json:"state"`
	Result    string `json:"result"`
}

// PendingChildQuestion keeps the child's finish metadata intact for a later answer.
type PendingChildQuestion struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Details   string `json:"details"`
}

func (l *cliLoop) latchQuota(err error) {
	if l.quota != nil || !agent.IsHardQuotaLimit(err) {
		return
	}
	reset, _ := agent.QuotaLimitResetTime(err, time.Now())
	l.quota = &cliQuotaStop{cause: err, reset: reset, terminal: map[string]bool{}}
	l.snapshotQuotaTerminal()
	fmt.Fprintf(l.errOut(), "rush run: session %q: hard provider quota exhausted; no further own turns; waiting only for active work\n", l.sessionID)
}

func (l *cliLoop) quotaPhase() cliStepResult {
	src, ok := l.source.(agent.CLIActiveWorkSource)
	if !ok {
		final, err := l.exit(fmt.Errorf("provider quota exhausted: active work reader unavailable: %w", l.quota.cause), "provider_limit")
		return cliStepResult{ev: evScopeStop, final: final, err: err}
	}
	var dbErrorRetryStart time.Time
	for {
		if err := l.stopError(); err != nil {
			final, exitErr := l.exitPrecheck(err)
			return cliStepResult{ev: evScopeStop, final: final, err: exitErr}
		}
		var active bool
		var err error
		if l.quota.snapshotIncomplete {
			l.snapshotQuotaTerminal()
			if l.quota.snapshotIncomplete {
				err = fmt.Errorf("quota terminal snapshot read incomplete")
			}
		}
		if err == nil {
			active, err = src.CLIActiveWork(l.ctx, l.sessionID)
			if cliQuotaActiveWorkSeam != nil {
				active, err = cliQuotaActiveWorkSeam(active, err)
			}
		}
		if err != nil {
			slog.Warn("rush run: active work check failed; retrying", "session_id", l.sessionID, "err", err)
			if dbErrorRetryStart.IsZero() {
				dbErrorRetryStart = time.Now()
			}
			if time.Since(dbErrorRetryStart) > cliDBErrorRetryOverallLimit {
				fmt.Fprintf(l.errOut(), "rush run: session %q's active work database has been unreadable for %s (%s); giving up\n", l.sessionID, cliDBErrorRetryOverallLimit, err)
				final, exitErr := l.exitWait(err)
				return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
			}
			if !sleepOrCtxDone(l.ctx, cliDBRetryPause) {
				final, exitErr := l.exitWait(l.ctx.Err())
				return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
			}
			continue
		}
		dbErrorRetryStart = time.Time{}
		if !active {
			break
		}
		if now := time.Now(); l.lastOpenScopeNotice.IsZero() || now.Sub(l.lastOpenScopeNotice) >= cliOpenScopeWaitNoticeInterval {
			l.lastOpenScopeNotice = now
			fmt.Fprintf(l.errOut(), "rush run: session %q still has active work after provider quota exhaustion; waiting on %s\n", l.sessionID, l.describeOpenWork())
		}
		l.source.WaitForHint(l.ctx, l.sessionID, time.Now().Add(time.Second))
	}
	l.cancelLoopSchedulesAtClose()
	if l.final == nil {
		l.final = &RunResult{SessionID: l.sessionID, ToolCalls: []ToolCallStat{}}
	}
	l.final.ResumeCommand = fmt.Sprintf("rush run --role %s --session %s", l.resumeRole(), l.sessionID)
	if !l.quota.reset.IsZero() {
		l.final.QuotaResetAt = l.quota.reset.Format(time.RFC3339)
	}
	rows, incomplete, err := l.quotaJobsInTree(l.ctx)
	if err != nil {
		final, exitErr := l.exitWait(err)
		return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
	}
	if incomplete {
		final, err := l.exitWait(fmt.Errorf("quota exit: finished work read incomplete"))
		return cliStepResult{ev: evScopeWaitErr, final: final, err: err}
	}
	children := map[string]bool{}
	for _, row := range rows {
		if l.quotaTerminalState(row.State) && row.Announced && !l.quota.terminal[row.Key] {
			l.final.FinishedWork = append(l.final.FinishedWork, FinishedWork{SessionID: row.OwnerSessionID, JobID: row.ToolCallID, State: row.State, Result: row.Result})
		}
		if row.ChildSessionID != "" {
			children[row.ChildSessionID] = true
		}
	}
	childIDs := make([]string, 0, len(children))
	for child := range children {
		childIDs = append(childIDs, child)
	}
	slices.Sort(childIDs)
	questions := make(map[string]session.ChildQuestion)
	if batchCutoverOn() {
		facts := agent.NewBatchCutoverNodeFacts(session.NewBatchStore(l.app.batchCutoverWriter()), nil, l.app.batchCutoverWriter())
		owners := make(map[string]bool)
		for _, row := range rows {
			if row.OwnerSessionID != "" {
				owners[row.OwnerSessionID] = true
			}
		}
		for owner := range owners {
			held, heldErr := facts.LiveHeldQuestions(l.ctx, owner)
			if heldErr != nil {
				final, exitErr := l.exitWait(heldErr)
				return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
			}
			for _, heldRow := range held {
				var noticeText string
				err := l.app.batchCutoverWriter().QueryRowContext(l.ctx, `SELECT text FROM session_notices WHERE id = ?1 AND owner = ?2 AND kind = ?3 AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0`, heldRow.ID, owner, session.NoticeKindChildQuestion).Scan(&noticeText)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						continue
					}
					final, exitErr := l.exitWait(err)
					return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
				}
				question, ok := session.ParseBatchChildQuestionNotice(noticeText)
				if ok && question.NodeID == heldRow.NodeID && question.ChildSessionID == heldRow.ChildSessionID {
					question.Question = quotaQuestionText(noticeText)
					questions[question.ChildSessionID] = question
				}
			}
		}
	}
	for _, child := range childIDs {
		if question, ok := questions[child]; ok {
			l.final.PendingChildQuestions = append(l.final.PendingChildQuestions, PendingChildQuestion{SessionID: child, Title: question.Question, Details: question.Question})
			continue
		}
		if batchCutoverOn() {
			continue
		}
		msgs, err := l.app.Messages.List(l.ctx, child)
		if err != nil {
			final, exitErr := l.exitWait(err)
			return cliStepResult{ev: evScopeWaitErr, final: final, err: exitErr}
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant || !msgs[i].IsFinished() {
				continue
			}
			fp := msgs[i].FinishPart()
			if fp != nil && agent.IsAwaitingAnswerFinish(fp) {
				l.final.PendingChildQuestions = append(l.final.PendingChildQuestions, PendingChildQuestion{SessionID: child, Title: fp.Message, Details: fp.Details})
			}
			break
		}
	}
	l.tot.subAgentOutputs = l.app.collectSubAgentOutputs(l.ctx, l.sessionID)
	text := "Hard provider quota exhausted: " + l.quota.cause.Error()
	if l.final.QuotaResetAt != "" {
		text += "\nLimit resets: " + l.final.QuotaResetAt
	}
	text += "\nResume: " + l.final.ResumeCommand
	text += "\nOwn once schedules were ignored for this run and remain persisted; they may fire on a later host."
	fmt.Fprintln(l.errOut(), text)
	for _, work := range l.final.FinishedWork {
		fmt.Fprintf(l.errOut(), "Finished %s/%s (%s): %s\n", work.SessionID, work.JobID, work.State, work.Result)
	}
	for _, q := range l.final.PendingChildQuestions {
		fmt.Fprintf(l.errOut(), "Pending child question %s: %s\n%s\n", q.SessionID, q.Title, q.Details)
	}
	l.tot.extraWarnings = append(l.tot.extraWarnings, "Own once schedules ignored for this run; persisted schedules may fire on a later host.")
	final, err := l.exit(&runIncompleteError{reason: "provider_limit", detail: text, cause: l.quota.cause}, "provider_limit")
	return cliStepResult{ev: evScopeStop, final: final, err: err}
}

func quotaQuestionText(notice string) string {
	const prefix = "SUB-AGENT QUESTION: "
	i := strings.Index(notice, prefix)
	if i < 0 {
		return ""
	}
	text := notice[i+len(prefix):]
	const guidance = "\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	return strings.TrimSuffix(text, guidance)
}

// cliQuotaActiveWorkSeam injects read failures in real-loop tests only.
var cliQuotaActiveWorkSeam func(bool, error) (bool, error)

func (l *cliLoop) snapshotQuotaTerminal() {
	rows, incomplete, err := l.quotaJobsInTree(l.ctx)
	l.quota.snapshotIncomplete = incomplete || err != nil
	if l.quota.snapshotIncomplete {
		return
	}
	for _, row := range rows {
		if l.quotaTerminalState(row.State) {
			l.quota.terminal[row.Key] = true
		}
	}
}

// quotaJobsInTree reads the finished-work rows of the session's tree: the
// legacy async_jobs walk flag-OFF, the batch root/node walk flag-ON
// (batchFinishedWork in app_batch_cutover_facts.go).
func (l *cliLoop) quotaJobsInTree(ctx context.Context) ([]quotaWorkRow, bool, error) {
	if batchCutoverOn() {
		return l.app.batchFinishedWork(ctx, l.sessionID)
	}
	rows, incomplete := l.app.asyncJobStore.JobsInTree(ctx, l.sessionID)
	out := make([]quotaWorkRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, quotaWorkRow{
			OwnerSessionID: row.OwnerSessionID,
			Key:            row.ClaimID,
			ToolCallID:     row.ToolCallID,
			State:          row.State,
			Announced:      row.Announced != 0,
			Result:         row.ResultSummary.String,
			ChildSessionID: row.ChildSessionID.String,
		})
	}
	return out, incomplete, nil
}

// quotaTerminalState is the terminal predicate: legacy "any non-running
// state"; batch the terminal node state set (a pending/starting/cancelling
// node is not finished).
func (l *cliLoop) quotaTerminalState(state string) bool {
	if batchCutoverOn() {
		switch state {
		case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
			return true
		}
		return false
	}
	return state != "running"
}

func (l *cliLoop) resumeRole() string {
	return runResumeRole(string(l.overrides.ModelRole))
}
