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
			if params.ChildSessionID == "" {
				delegations, err := control.ListDelegations(ctx, sessionID)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				switch len(delegations) {
				case 0:
					return fantasy.NewTextResponse("No live sub-agent delegations for this session. Nothing to inspect; start one with the agent tool."), nil
				case 1:
					insp, err := control.InspectAgent(ctx, sessionID, delegations[0].ChildSessionID)
					if err != nil {
						return fantasy.NewTextErrorResponse(err.Error()), nil
					}
					data, err := json.Marshal(insp)
					if err != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent output: %w", err)
					}
					return fantasy.NewTextResponse(string(data)), nil
				default:
					data, err := json.Marshal(delegations)
					if err != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent listing: %w", err)
					}
					return fantasy.NewTextResponse("Multiple live sub-agent delegations; call again with child_session_id from this list:\n" + string(data)), nil
				}
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
