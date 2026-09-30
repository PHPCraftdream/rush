package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

const InspectAgentToolName = "inspect_agent"

//go:embed inspect_agent.md
var inspectAgentDescription string

type InspectAgentParams struct {
	childSessionIDParam
}

// NewInspectAgentTool builds inspect_agent (contract §4.1): a read-only
// status probe over a delegated child session.
func NewInspectAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		InspectAgentToolName,
		inspectAgentDescription,
		func(ctx context.Context, params InspectAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to inspect a sub-agent"), nil
			}
			childSessionID, err := childSessionFromParams(params.childSessionIDParam)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			insp, err := control.InspectAgent(ctx, sessionID, childSessionID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			data, err := json.Marshal(insp)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent output: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}
