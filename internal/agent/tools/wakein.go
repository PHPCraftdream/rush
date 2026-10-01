package tools

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"charm.land/fantasy"
)

const WakeinToolName = "wakein"

//go:embed wakein.md
var wakeinDescription string

// NewWakeinTool builds wakein (contract §1.1): a one-shot wake after a delay.
func NewWakeinTool(control WakeControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WakeinToolName,
		wakeinDescription,
		func(ctx context.Context, params WakeinParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to schedule a wake"), nil
			}
			view, err := control.ScheduleWakeOnceDelay(ctx, sessionID, params.DelaySeconds, params.Message)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text := fmt.Sprintf(
				"Wake %s scheduled for %s (in %s). Use wake_list to see it, wake_cancel to cancel it.",
				view.ScheduleID, view.FiresAt, time.Duration(params.DelaySeconds)*time.Second,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), view), nil
		},
	)
}
