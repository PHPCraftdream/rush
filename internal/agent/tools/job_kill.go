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
	ShellID string `json:"shell_id" description:"The ID of the background shell to terminate"`
}

type JobKillResponseMetadata struct {
	ShellID     string `json:"shell_id"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

func NewJobKillTool(managers ...*shell.BackgroundShellManager) fantasy.AgentTool {
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
			if params.ShellID == "" {
				return fantasy.NewTextErrorResponse("missing shell_id"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if owned && sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required for background shell ownership"), nil
			}

			var bgShell *shell.BackgroundShell
			var ok bool
			if owned {
				bgShell, ok = bgManager.GetOwned(sessionID, params.ShellID)
			} else {
				bgShell, ok = bgManager.Get(params.ShellID)
			}
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("background shell not found: %s", params.ShellID)), nil
			}

			metadata := JobKillResponseMetadata{
				ShellID:     params.ShellID,
				Command:     bgShell.Command,
				Description: bgShell.Description,
			}

			var err error
			if owned {
				err = bgManager.KillOwned(ctx, sessionID, params.ShellID)
			} else {
				err = bgManager.Kill(ctx, params.ShellID)
			}
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			result := fmt.Sprintf("Background shell %s terminated successfully", params.ShellID)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}
