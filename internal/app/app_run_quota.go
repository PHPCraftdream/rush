package app

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
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
	rows, incomplete := l.app.asyncJobStore.JobsInTree(l.ctx, l.sessionID)
	if incomplete {
		final, err := l.exitWait(fmt.Errorf("quota exit: finished work read incomplete"))
		return cliStepResult{ev: evScopeWaitErr, final: final, err: err}
	}
	children := map[string]bool{}
	for _, row := range rows {
		if row.State != "running" && row.Announced != 0 && !l.quota.terminal[row.ClaimID] {
			l.final.FinishedWork = append(l.final.FinishedWork, FinishedWork{SessionID: row.OwnerSessionID, JobID: row.ToolCallID, State: row.State, Result: row.ResultSummary.String})
		}
		if row.ChildSessionID.Valid {
			children[row.ChildSessionID.String] = true
		}
	}
	childIDs := make([]string, 0, len(children))
	for child := range children {
		childIDs = append(childIDs, child)
	}
	slices.Sort(childIDs)
	for _, child := range childIDs {
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

// cliQuotaActiveWorkSeam injects read failures in real-loop tests only.
var cliQuotaActiveWorkSeam func(bool, error) (bool, error)

func (l *cliLoop) snapshotQuotaTerminal() {
	rows, incomplete := l.app.asyncJobStore.JobsInTree(l.ctx, l.sessionID)
	l.quota.snapshotIncomplete = incomplete
	if incomplete {
		return
	}
	for _, row := range rows {
		if row.State != "running" {
			l.quota.terminal[row.ClaimID] = true
		}
	}
}

func (l *cliLoop) resumeRole() string {
	if l.overrides.ModelRole != "" {
		return string(l.overrides.ModelRole)
	}
	return "smart"
}
