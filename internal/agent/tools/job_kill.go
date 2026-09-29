package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
)

const (
	JobKillToolName = "job_kill"
)

//go:embed job_kill.md
var jobKillDescription string

type JobKillParams struct {
	JobID   string `json:"job_id,omitempty" description:"The async job id returned when the command started (e.g. from \"Async bash job <id> started\"). Preferred over shell_id. Exactly one of job_id/shell_id is required."`
	ShellID string `json:"shell_id,omitempty" description:"The ID of the background shell to terminate, when you have a raw shell id instead of a job id. Exactly one of job_id/shell_id is required."`
}

type JobKillResponseMetadata struct {
	JobID       string `json:"job_id,omitempty"`
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

// NewJobKillTool builds the job_kill tool. resolver resolves a job_id (the
// async job id the model saw) to a shell id -- nil disables job_id support,
// leaving shell_id as the only way to address a job (see resolveShellID).
// runCtl controls a run_command job_id (task #1023 §3) -- nil disables
// run_command job control, leaving RunCommandJobError's text as the final
// answer for one.
func NewJobKillTool(resolver JobShellResolver, runCtl RunCommandController, managers ...*shell.BackgroundShellManager) fantasy.AgentTool {
	owned := false
	var bgManager *shell.BackgroundShellManager
	if len(managers) > 0 && managers[0] != nil {
		bgManager = managers[0]
		owned = true
	} else {
		bgManager = shell.NewBackgroundShellManager()
	}
	return fantasy.NewAgentTool(
		JobKillToolName,
		jobKillDescription,
		func(ctx context.Context, params JobKillParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if owned && sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required for background shell ownership"), nil
			}

			shellID, err := resolveShellID(resolver, sessionID, params.JobID, params.ShellID)
			if err != nil {
				var rcErr *RunCommandJobError
				if errors.As(err, &rcErr) && runCtl != nil {
					// Task #1063: the result IS the tool's own answer, with
					// the real output snapshotted before the stop -- not a
					// placeholder promising a later message (job_kill now
					// produces no second notice for a run_command job
					// either).
					stopText, stopErr := runCtl.StopRunCommandJob(sessionID, params.JobID)
					if stopErr != nil {
						return fantasy.NewTextErrorResponse(stopErr.Error()), nil
					}
					if stopText == "" {
						stopText = fmt.Sprintf("Async job %s (run_command) kill requested; it will stop shortly and its result will arrive as a message.", params.JobID)
					}
					metadata := JobKillResponseMetadata{JobID: params.JobID}
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stopText), metadata), nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Task #1063: markText, on JobStopStopped, IS the tool's own
			// final answer -- the real output snapshot taken BEFORE the kill
			// below, already worded to distinguish this cause (stopped/
			// job_kill) from Stop-cancel or a timeout. The pre-existing
			// generic "terminated successfully" wording is reachable only
			// when job_id was not given at all (a raw shell_id call, never
			// ledger-tracked).
			//
			// B11: the ledger verdict is consulted BEFORE the shell manager
			// is touched at all -- not even to look the shell up -- so a
			// lost race or a gone job never surfaces the manager's generic
			// "background shell not found" wording, and never kills a shell
			// the ledger has already moved past.
			var markText string
			var marked bool
			if resolver != nil && params.JobID != "" {
				// Records the ledger's stop-on-request marker BEFORE the
				// kill below, so the job's own finish() call (task #1023
				// §2.2) produces a distinct "stopped (job_kill)" outcome
				// instead of describing the killed process's own exit.
				var verdict JobStopVerdict
				markText, verdict = resolver.MarkJobStopped(sessionID, params.JobID)
				switch verdict {
				case JobStopAlreadyTerminal:
					// This call lost the race -- the ledger row already
					// reached a terminal state via another cause (natural
					// finish, timeout, Stop). Answer from the COMMITTED row.
					// No JobKillResponseMetadata on purpose: nothing for the
					// A3 result fusion to record.
					return fantasy.NewTextResponse(markText), nil
				case JobStopStopped:
					marked = true
				default: // JobStopNotFound
					// Not a live tracked job (already delivered, or a
					// concurrent job_kill already claimed it): idempotent
					// refusal (contract §1.5).
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"job %s not found (not owned by this session, already delivered, or already stopped)", params.JobID,
					)), nil
				}
			}

			var bgShell *shell.BackgroundShell
			var ok bool
			if owned {
				bgShell, ok = bgManager.GetOwned(sessionID, shellID)
			} else {
				bgShell, ok = bgManager.Get(shellID)
			}
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", shellID)), nil
			}

			metadata := JobKillResponseMetadata{
				JobID:       params.JobID,
				ShellID:     shellID,
				Command:     bgShell.Command,
				Description: bgShell.Description,
			}

			if owned {
				err = bgManager.KillOwned(ctx, sessionID, shellID)
			} else {
				err = bgManager.Kill(ctx, shellID)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if marked && markText != "" {
				return fantasy.WithResponseMetadata(fantasy.NewTextResponse(markText), metadata), nil
			}
			result := fmt.Sprintf("Background shell %s terminated successfully", shellID)
			if params.JobID != "" {
				result = fmt.Sprintf("Background job %s (shell %s) terminated successfully", params.JobID, shellID)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
