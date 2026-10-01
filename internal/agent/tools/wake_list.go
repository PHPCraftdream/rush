package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

const WakeListToolName = "wake_list"

//go:embed wake_list.md
var wakeListDescription string

// NewWakeListTool builds wake_list (contract §1.4): this session's active
// schedules only. An empty list is not an error (§1.4).
func NewWakeListTool(control WakeControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WakeListToolName,
		wakeListDescription,
		func(ctx context.Context, params WakeListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to list wake schedules"), nil
			}
			views, err := control.ListWakeSchedules(ctx, sessionID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(views) == 0 {
				return fantasy.NewTextResponse("No active schedules for this session."), nil
			}
			data, err := json.MarshalIndent(struct {
				Schedules []WakeScheduleView `json:"schedules"`
			}{views}, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("marshal wake_list output: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}
