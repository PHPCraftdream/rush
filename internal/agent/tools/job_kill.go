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
					if stopErr := runCtl.StopRunCommandJob(sessionID, params.JobID); stopErr != nil {
						return fantasy.NewTextErrorResponse(stopErr.Error()), nil
					}
					result := fmt.Sprintf("Async job %s (run_command) kill requested; it will stop shortly and its result will arrive as a message.", params.JobID)
					metadata := JobKillResponseMetadata{JobID: params.JobID}
					return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
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

			if resolver != nil && params.JobID != "" {
				// Records the ledger's stop-on-request marker BEFORE the
				// kill below, so the job's own finish() call (task #1023
				// §2.2) produces a distinct "stopped (job_kill)" notice
				// instead of describing the killed process's own exit.
				resolver.MarkJobStopped(sessionID, params.JobID)
			}

			if owned {
				err = bgManager.KillOwned(ctx, sessionID, shellID)
			} else {
				err = bgManager.Kill(ctx, shellID)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			result := fmt.Sprintf("Background shell %s terminated successfully", shellID)
			if params.JobID != "" {
				result = fmt.Sprintf("Background job %s (shell %s) terminated successfully", params.JobID, shellID)
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
