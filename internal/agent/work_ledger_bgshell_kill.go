package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

// bgShellStopBudget bounds job_kill's store work for a bg_shell row.
const bgShellStopBudget = 10 * time.Second

// StopBackgroundShellRow implements tools.BGShellStopper (#1189): job_kill on a
// shell that has a durable bg_shell row (a shell started by a SYNC bash call,
// R-BG-1) but no in-memory ledger job, so MarkJobStopped cannot see it.
//
// The row's terminal "cancelled" transition (notice_kind job_kill,
// delivery=done, reacted=1, wake=0 -- the same contract as causeJobKill) is
// committed BEFORE job_kill kills the process. That order closes two defects:
// a process that refuses to die (R8B-2: a child holding the pipe) can no
// longer leave the row running forever, because the row does not wait for the
// process; and the process's own exit finds the row already terminal, so the
// observer (finishBGShellRow) and the completion callback
// (bgShellRowCancelled) both stand down instead of reporting "finished: exit 1"
// and waking the session a second time.
//
// Only a shell this process owns is stopped: the row of a shell on another
// host cannot be killed from here, so it stays the dead-host sweep's business.
func (l *workLedger) StopBackgroundShellRow(owner, shellID string) (text, claimID string, verdict tools.JobStopVerdict) {
	if l == nil {
		return "", "", tools.JobStopNotFound
	}
	l.mu.Lock()
	store := l.store
	l.mu.Unlock()
	if store == nil || l.coord == nil || l.coord.background == nil {
		return "", "", tools.JobStopNotFound
	}
	if _, ok := l.coord.background.GetOwned(owner, shellID); !ok {
		return "", "", tools.JobStopNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), bgShellStopBudget)
	defer cancel()
	row, err := store.Get(ctx, owner, shellID)
	if err != nil || session.JobKind(row.Kind) != session.JobKindBGShell {
		return "", "", tools.JobStopNotFound
	}
	if row.HostID != store.HostID() {
		// Another process's shell row (cross-process id collision): not ours
		// to cancel, and the shell is not in this process to kill.
		return "", "", tools.JobStopNotFound
	}
	if row.State != "running" {
		return FormatAsyncCompletion(AsyncCompletion{
			ToolCallID: shellID, ToolName: tools.BashToolName,
			Content: row.ResultSummary.String, IsError: row.ResultIsError.Int64 != 0,
		}), "", tools.JobStopAlreadyTerminal
	}

	partial := l.capturePartial(owner, shellID, tools.BashToolName, "", shellID, nil)
	var result session.TransitionResult
	if err := l.retryAsyncStoreOp(ctx, func() error {
		var transitionErr error
		result, transitionErr = store.Transition(ctx, session.TransitionParams{
			Owner: owner, ToolCallID: shellID, State: "cancelled", NoticeKind: "job_kill",
			ResultSummary: partial.content, ResultIsError: partial.isError,
			Wake: false, Delivery: "done", Reacted: true, ClaimID: row.ClaimID,
		})
		return transitionErr
	}); err != nil {
		// The row could not be written: let job_kill take its older path (kill
		// the process, the observer closes the row when it exits).
		slog.Error("job_kill: failed to cancel the background-shell row", "session_id", owner, "shell_id", shellID, "err", err)
		return "", "", tools.JobStopNotFound
	}
	switch result.Outcome {
	case session.TransitionWon:
		return FormatAsyncCompletion(AsyncCompletion{
			ToolCallID: shellID, ToolName: tools.BashToolName,
			Content: partial.content, IsError: partial.isError, Stopped: true,
		}), row.ClaimID, tools.JobStopStopped
	case session.TransitionLost:
		// Lost the race to the process's own exit (or another stop): answer from
		// the committed row.
		return FormatAsyncCompletion(AsyncCompletion{
			ToolCallID: shellID, ToolName: tools.BashToolName,
			Content: result.Row.ResultSummary.String, IsError: result.Row.ResultIsError.Int64 != 0,
		}), "", tools.JobStopAlreadyTerminal
	}
	return "", "", tools.JobStopNotFound
}

// bgShellRowCancelled reports whether shellID's bg_shell row was already
// cancelled by job_kill: its completion must then produce neither a wake
// notice nor an auto-resume slot, because the job_kill result IS its answer.
func (c *coordinator) bgShellRowCancelled(sessionID, shellID string) bool {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), bgShellStopBudget)
	defer cancel()
	row, err := c.asyncJobs.store.Get(ctx, sessionID, shellID)
	return err == nil && session.JobKind(row.Kind) == session.JobKindBGShell && row.State == "cancelled"
}
