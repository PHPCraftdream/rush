package tools

import (
	"context"
	_ "embed"
	"fmt"

	"charm.land/fantasy"
)

const WakeonToolName = "wakeon"

//go:embed wakeon.md
var wakeonDescription string

// NewWakeonTool builds wakeon (contract §1.2): a one-shot wake at an RFC3339
// timestamp with an explicit zone. fires_at in the response is normalized to
// UTC (§1.2), so wakeon results compare without timezone arithmetic.
func NewWakeonTool(control WakeControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WakeonToolName,
		wakeonDescription,
		func(ctx context.Context, params WakeonParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to schedule a wake"), nil
			}
			view, err := control.ScheduleWakeOnceAt(ctx, sessionID, params.At, params.Message)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text := fmt.Sprintf(
				"Wake %s scheduled for %s. Use wake_list to see it, wake_cancel to cancel it.",
				view.ScheduleID, view.FiresAt,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), view), nil
		},
	)
}
