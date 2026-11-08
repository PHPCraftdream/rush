package tools

import (
	"context"
	_ "embed"

	"charm.land/fantasy"
)

const StopAgentToolName = "stop_agent"

//go:embed stop_agent.md
var stopAgentDescription string

type StopAgentParams struct {
	ChildSessionID string `json:"child_session_id" description:"Full delegated child session id, including any $$call_ suffix. Take it from the child_question notice or inspect_agent listing."`
}

// NewStopAgentTool builds stop_agent (contract §4.3): stops ONLY the child's
// current turn and armed delegation; history is preserved.
func NewStopAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StopAgentToolName,
		stopAgentDescription,
		func(ctx context.Context, params StopAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to stop a sub-agent"), nil
			}
			childSessionID, err := childSessionFromParams(childSessionIDParam(params))
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text, err := control.StopAgent(ctx, sessionID, childSessionID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(text), nil
		},
	)
}
