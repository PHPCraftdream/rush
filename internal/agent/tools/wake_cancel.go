package tools

import (
	"context"
	_ "embed"

	"charm.land/fantasy"
)

const WakeCancelToolName = "wake_cancel"

//go:embed wake_cancel.md
var wakeCancelDescription string

// WakeCancelResponseMetadata is wake_cancel's structured success payload
// (§1.5): {"schedule_id": "...", "cancelled": true}.
type WakeCancelResponseMetadata struct {
	ScheduleID string `json:"schedule_id"`
	Cancelled  bool   `json:"cancelled"`
}

// NewWakeCancelTool builds wake_cancel (contract §1.5). Idempotency rule of
// §1.5: a repeat on an inactive/foreign/never-existing id returns the SAME
// "not found" error — a safe no-op, never a disguised success.
func NewWakeCancelTool(control WakeControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WakeCancelToolName,
		wakeCancelDescription,
		func(ctx context.Context, params WakeCancelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to cancel a wake schedule"), nil
			}
			if params.ScheduleID == "" {
				return fantasy.NewTextErrorResponse("missing schedule_id"), nil
			}
			text, err := control.CancelWakeSchedule(ctx, sessionID, params.ScheduleID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			metadata := WakeCancelResponseMetadata{ScheduleID: params.ScheduleID, Cancelled: true}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), metadata), nil
		},
	)
}
