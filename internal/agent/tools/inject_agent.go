package tools

import (
	"context"
	_ "embed"
	"strings"

	"charm.land/fantasy"
)

const InjectAgentToolName = "inject_agent"

//go:embed inject_agent.md
var injectAgentDescription string

type InjectAgentParams struct {
	childSessionIDParam
	Message   string `json:"message" description:"The message to deliver into the sub-agent's session."`
	Interrupt bool   `json:"interrupt,omitempty" description:"Cancel the sub-agent's current turn and hand it this message immediately instead of waiting for the turn to end. Default false."`
}

// NewInjectAgentTool builds inject_agent (contract §4.2).
func NewInjectAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		InjectAgentToolName,
		injectAgentDescription,
		func(ctx context.Context, params InjectAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to inject into a sub-agent"), nil
			}
			childSessionID, err := childSessionFromParams(params.childSessionIDParam)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if strings.TrimSpace(params.Message) == "" {
				return fantasy.NewTextErrorResponse("message is required"), nil
			}
			text, err := control.InjectAgent(ctx, sessionID, childSessionID, params.Message, params.Interrupt)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(text), nil
		},
	)
}
