package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

const AwaitTasksToolName = "await_tasks"

//go:embed await_tasks.md
var awaitTasksDescription string

//go:embed await_tasks_batch.md
var awaitTasksBatchDescription string

type AwaitTasksParams struct {
	Until string `json:"until,omitempty" description:"\"any\" (default) wakes on the FIRST completion; \"all\" sleeps until every task has finished."`
	// MaxWaitSeconds is optional; 0 means no deadline (sleep until the work itself wakes the session).
	MaxWaitSeconds int    `json:"max_wait_seconds,omitempty" description:"Optional safety deadline in seconds (60..21600); a one-shot wake fires then, even if the work is still running."`
	Note           string `json:"note,omitempty" description:"Optional short note about why you are waiting (recorded in the result for your later self)."`
}
type AwaitTaskRef struct {
	ToolName       string `json:"tool_name"`
	ToolCallID     string `json:"tool_call_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	AgeSeconds     int64  `json:"age_seconds"`
	BatchRootID    string `json:"batch_root_id,omitempty"`
	BatchNodeID    string `json:"batch_node_id,omitempty"`
}
type AwaitTasksResult struct {
	Mode              string         `json:"mode"`
	MaxWaitScheduleID string         `json:"max_wait_schedule_id,omitempty"`
	WaitingFor        []AwaitTaskRef `json:"waiting_for"`
	Blocked           bool           `json:"blocked,omitempty"`
	TimedOut          bool           `json:"timed_out,omitempty"`
	FinishedCount     int            `json:"finished_count,omitempty"`
}
type AwaitControl interface {
	AwaitTasks(context.Context, string, string, int) (AwaitTasksResult, error)
}

func NewAwaitTasksTool(c AwaitControl) fantasy.AgentTool {
	return newAwaitTasksTool(c, awaitTasksDescription, false)
}

func NewBatchAwaitTasksTool(c AwaitControl) fantasy.AgentTool {
	return newAwaitTasksTool(c, awaitTasksBatchDescription, true)
}

func newAwaitTasksTool(control AwaitControl, description string, showBatchIDs bool) fantasy.AgentTool {
	return fantasy.NewAgentTool(AwaitTasksToolName, description, func(ctx context.Context, params AwaitTasksParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		mode := strings.TrimSpace(params.Until)
		if mode == "" {
			mode = "any"
		}
		if mode != "any" && mode != "all" {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("until must be \"any\" or \"all\" (default \"any\"), got %q", params.Until)), nil
		}
		if params.MaxWaitSeconds < 0 || params.MaxWaitSeconds > 21600 {
			return fantasy.NewTextErrorResponse("max_wait_seconds must be between 60 and 21600 (6 hours), or omitted"), nil
		}
		res, err := control.AwaitTasks(ctx, GetSessionFromContext(ctx), mode, params.MaxWaitSeconds)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if res.Blocked {
			lines := make([]string, 0, len(res.WaitingFor)+4)
			lines = append(lines, fmt.Sprintf("Woke: %d task(s) finished.", res.FinishedCount))
			if res.TimedOut {
				lines = append(lines, "The max_wait_seconds deadline elapsed before the work finished.")
			}
			if len(res.WaitingFor) > 0 {
				lines = append(lines, "Still running:")
				for _, ref := range res.WaitingFor {
					lines = append(lines, renderAwaitRef(ref, showBatchIDs, showBatchIDs))
				}
				if showBatchIDs && hasAwaitBatchRoot(res.WaitingFor) {
					lines = append(lines, "Use batch_status/batch_output/batch_stop to inspect or control batch work.")
				}
				lines = append(lines, "Call await_tasks again to keep waiting; do not end your turn while jobs you still need are running.")
			}
			if strings.TrimSpace(params.Note) != "" {
				lines = append(lines, "Note: "+strings.TrimSpace(params.Note))
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(strings.Join(lines, "\n")), res), nil
		}
		lines := make([]string, 0, len(res.WaitingFor)+4)
		lines = append(lines, fmt.Sprintf("Sleeping (%s): waiting for %d task(s).", res.Mode, len(res.WaitingFor)))
		if strings.TrimSpace(params.Note) != "" {
			lines = append(lines, "Note: "+strings.TrimSpace(params.Note))
		}
		for _, ref := range res.WaitingFor {
			entry := fmt.Sprintf("- %s (tool_call_id %s, running for %ds)", ref.ToolName, ref.ToolCallID, ref.AgeSeconds)
			if ref.ChildSessionID != "" {
				entry += fmt.Sprintf(", child session %s", ref.ChildSessionID)
			}
			if showBatchIDs && ref.BatchRootID != "" {
				entry += fmt.Sprintf(", batch root_id %s node_id %s", ref.BatchRootID, ref.BatchNodeID)
			}
			lines = append(lines, entry)
		}
		if showBatchIDs && hasAwaitBatchRoot(res.WaitingFor) {
			lines = append(lines, "Use batch_status/batch_output/batch_stop to inspect or control batch work.")
		}
		if res.MaxWaitScheduleID != "" {
			lines = append(lines, fmt.Sprintf("A wake fires in at most %d seconds (schedule %s) even if they are still running.", params.MaxWaitSeconds, res.MaxWaitScheduleID))
		}
		lines = append(lines, "This turn now ends; you will be woken as the tasks finish. Do not poll.")
		resp := fantasy.WithResponseMetadata(fantasy.NewTextResponse(strings.Join(lines, "\n")), res)
		resp.StopTurn = true
		return resp, nil
	})
}

func hasAwaitBatchRoot(refs []AwaitTaskRef) bool {
	for _, ref := range refs {
		if ref.BatchRootID != "" {
			return true
		}
	}
	return false
}

func renderAwaitRef(ref AwaitTaskRef, showBatch, showChild bool) string {
	entry := fmt.Sprintf("- %s (tool_call_id %s, running for %ds)", ref.ToolName, ref.ToolCallID, ref.AgeSeconds)
	if showChild && ref.ChildSessionID != "" {
		entry += fmt.Sprintf(", child session %s", ref.ChildSessionID)
	}
	if showBatch && ref.BatchRootID != "" {
		entry += fmt.Sprintf(", batch root_id %s node_id %s", ref.BatchRootID, ref.BatchNodeID)
	}
	return entry
}
