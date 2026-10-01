package tools

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"charm.land/fantasy"
)

const WakeLoopToolName = "loop"

//go:embed wake_loop.md
var wakeLoopDescription string

// NewWakeLoopTool builds loop (contract §1.3): a recurring wake on a fixed
// interval, bounded by optional max_runs/until.
func NewWakeLoopTool(control WakeControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WakeLoopToolName,
		wakeLoopDescription,
		func(ctx context.Context, params WakeLoopParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to schedule a loop"), nil
			}
			view, err := control.ScheduleWakeLoop(ctx, sessionID, params.EverySeconds, params.Message, params.MaxRuns, params.Until)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text := fmt.Sprintf(
				"Loop %s scheduled: every %s, first occurrence at %s. Use wake_list to see it, wake_cancel to cancel it.",
				view.ScheduleID, time.Duration(view.EverySeconds)*time.Second, view.NextFireAt,
			)
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), view), nil
		},
	)
}
