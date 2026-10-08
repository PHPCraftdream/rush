package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// AwaitTasksToolName is the tool name the model calls to end its turn and
// sleep until its live background work reports back.
const AwaitTasksToolName = "await_tasks"

//go:embed await_tasks.md
var awaitTasksDescription string

// AwaitTasksParams is the JSON schema for the await_tasks tool.
type AwaitTasksParams struct {
	Until string `json:"until,omitempty" description:"\"any\" (default) wakes on the FIRST completion; \"all\" sleeps until every task has finished."`
	// MaxWaitSeconds is optional; 0 means no deadline (sleep until the work
	// itself wakes the session). Non-zero values must be 60..21600.
	MaxWaitSeconds int    `json:"max_wait_seconds,omitempty" description:"Optional safety deadline in seconds (60..21600); a one-shot wake fires then, even if the work is still running."`
	Note           string `json:"note,omitempty" description:"Optional short note about why you are waiting (recorded in the result for your later self)."`
}

// AwaitTaskRef is one live task the session is waiting on.
type AwaitTaskRef struct {
	ToolName       string `json:"tool_name"`
	ToolCallID     string `json:"tool_call_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	AgeSeconds     int64  `json:"age_seconds"`
}

// AwaitTasksResult is the coordinator's answer the tool renders.
type AwaitTasksResult struct {
	Mode              string         `json:"mode"`
	MaxWaitScheduleID string         `json:"max_wait_schedule_id,omitempty"`
	WaitingFor        []AwaitTaskRef `json:"waiting_for"`
	// Blocked marks the delegated-worker path: the call blocked inside the
	// turn and returned on its own; the turn does NOT end.
	Blocked bool `json:"blocked,omitempty"`
	// TimedOut marks a worker wake caused by the max_wait/wait-cap deadline.
	TimedOut bool `json:"timed_out,omitempty"`
	// FinishedCount (worker path only) is how many awaited tasks completed
	// while the call blocked.
	FinishedCount int `json:"finished_count,omitempty"`
}

// AwaitControl is the coordinator-side half of the await_tasks tool. Like
// WakeControl, the tool layer is thin: it reads the caller session from
// context and delegates to AwaitControl, implemented by the coordinator
// (coordinator_await_tasks.go). Every returned error's message is model-safe
// (returned via NewTextErrorResponse, not a fatal Go error).
type AwaitControl interface {
	// AwaitTasks inspects the session's live work (own async jobs plus live
	// delegations), arms the "until: all" sleep when asked, schedules the
	// optional max_wait wake, and reports what the session will wait for.
	AwaitTasks(ctx context.Context, sessionID, mode string, maxWaitSeconds int) (AwaitTasksResult, error)
}

// NewAwaitTasksTool builds await_tasks: on success it returns a StopTurn
// response (fantasy.ToolResponse.StopTurn = true), which ends the turn the
// same way job_output's timed-out wait does — the session stays alive and
// the run continues automatically when the awaited work completes. It never
// suspends auto-resume and never produces awaiting_answer.
func NewAwaitTasksTool(control AwaitControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AwaitTasksToolName,
		awaitTasksDescription,
		func(ctx context.Context, params AwaitTasksParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			mode := strings.TrimSpace(params.Until)
			if mode == "" {
				mode = "any"
			}
			if mode != "any" && mode != "all" {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"until must be \"any\" or \"all\" (default \"any\"), got %q", params.Until)), nil
			}
			if params.MaxWaitSeconds < 0 || params.MaxWaitSeconds > 21600 {
				return fantasy.NewTextErrorResponse(
					"max_wait_seconds must be between 60 and 21600 (6 hours), or omitted"), nil
			}
			sessionID := GetSessionFromContext(ctx)
			res, err := control.AwaitTasks(ctx, sessionID, mode, params.MaxWaitSeconds)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if res.Blocked {
				// Worker (delegated child): the call blocked inside this turn;
				// the turn continues so the delegation stays open.
				lines := make([]string, 0, len(res.WaitingFor)+4)
				lines = append(lines, fmt.Sprintf("Woke: %d task(s) finished.", res.FinishedCount))
				if res.TimedOut {
					lines = append(lines, "The max_wait_seconds deadline elapsed before the work finished.")
				}
				if len(res.WaitingFor) > 0 {
					lines = append(lines, "Still running:")
					for _, ref := range res.WaitingFor {
						lines = append(lines, fmt.Sprintf("- %s (tool_call_id %s, running for %ds)",
							ref.ToolName, ref.ToolCallID, ref.AgeSeconds))
					}
					lines = append(lines,
						"Call await_tasks again to keep waiting; do not end your turn while jobs you still need are running.")
				}
				if strings.TrimSpace(params.Note) != "" {
					lines = append(lines, "Note: "+strings.TrimSpace(params.Note))
				}
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(strings.Join(lines, "\n")), res), nil
			}
			lines := make([]string, 0, len(res.WaitingFor)+4)
			lines = append(lines, fmt.Sprintf("Sleeping (%s): waiting for %d task(s).",
				res.Mode, len(res.WaitingFor)))
			if strings.TrimSpace(params.Note) != "" {
				lines = append(lines, "Note: "+strings.TrimSpace(params.Note))
			}
			for _, ref := range res.WaitingFor {
				entry := fmt.Sprintf("- %s (tool_call_id %s, running for %ds)",
					ref.ToolName, ref.ToolCallID, ref.AgeSeconds)
				if ref.ChildSessionID != "" {
					entry += fmt.Sprintf(", child session %s", ref.ChildSessionID)
				}
				lines = append(lines, entry)
			}
			if res.MaxWaitScheduleID != "" {
				lines = append(lines, fmt.Sprintf(
					"A wake fires in at most %d seconds (schedule %s) even if they are still running.",
					params.MaxWaitSeconds, res.MaxWaitScheduleID))
			}
			lines = append(lines,
				"This turn now ends; you will be woken as the tasks finish. Do not poll.")
			resp := fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(strings.Join(lines, "\n")), res)
			resp.StopTurn = true
			return resp, nil
		},
	)
}
