package tools

import (
	"context"
	_ "embed"
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
func NewJobKillTool(resolver JobShellResolver, managers ...*shell.BackgroundShellManager) fantasy.AgentTool {
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
