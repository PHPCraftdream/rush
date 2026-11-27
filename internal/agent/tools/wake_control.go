// Wake-schedule control tools (wakein/wakeon/loop/wake_list/wake_cancel,
// docs/plans/2026-09-27-wake-tools-contract.md §1). Like AgentControl, the
// tool layer is thin: it reads the caller session from context and delegates
// to WakeControl, implemented by the coordinator (coordinator_wake_tools.go)
// over the stage-4a WakeScheduleStore and the stage-4b worker's timer.
package tools

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// WakeControl is the coordinator-side half of the wake tools (contract §1).
// Every returned error's message is model-safe (returned via
// NewTextErrorResponse, not a fatal Go error).
type WakeControl interface {
	// ScheduleWakeOnceDelay implements wakein (§1.1): one-shot after
	// delaySeconds seconds.
	ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (WakeScheduleView, error)
	// ScheduleWakeOnceAt implements wakeon (§1.2): one-shot at an RFC3339
	// timestamp with an explicit zone.
	ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (WakeScheduleView, error)
	// ScheduleWakeLoop implements loop (§1.3): recurring every everySeconds,
	// bounded by optional maxRuns/until. maxRuns/until are nil when absent.
	ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (WakeScheduleView, error)
	// ListWakeSchedules implements wake_list (§1.4): THIS session's active
	// schedules only.
	ListWakeSchedules(ctx context.Context, sessionID string) ([]WakeScheduleView, error)
	// CancelWakeSchedule implements wake_cancel (§1.5) and returns the
	// tool's own answer text.
	CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error)
}

// WakeScheduleView is the JSON shape the wake tools return (§1 outputs):
// schedule_id, kind, and the next firing time in UTC RFC3339.
type WakeScheduleView struct {
	ScheduleID   string  `json:"schedule_id"`
	Kind         string  `json:"kind"`
	FiresAt      string  `json:"fires_at,omitempty"`
	EverySeconds int64   `json:"every_seconds,omitempty"`
	NextFireAt   string  `json:"next_fire_at,omitempty"`
	Occurrence   int64   `json:"occurrence,omitempty"`
	MaxRuns      *int64  `json:"max_runs,omitempty"`
	Until        *string `json:"until,omitempty"`
	Message      string  `json:"message"`
}

// WakeinParams is wakein's input (§1.1).
type WakeinParams struct {
	DelaySeconds int64  `json:"delay_seconds" description:"Whole seconds from now until the wake fires (5..3155760000)."`
	Message      string `json:"message" description:"Text delivered back to you when the wake fires (1..4000 characters)."`
}

// WakeonParams is wakeon's input (§1.2).
type WakeonParams struct {
	At      string `json:"at" description:"RFC3339 timestamp with an explicit offset or Z, e.g. 2026-09-28T09:00:00+03:00."`
	Message string `json:"message" description:"Text delivered back to you when the wake fires (1..4000 characters)."`
}

// WakeLoopParams is loop's input (§1.3).
type WakeLoopParams struct {
	EverySeconds int64   `json:"every_seconds" description:"Interval between occurrences in seconds (minimum 300)."`
	Message      string  `json:"message" description:"Text delivered on each occurrence (1..4000 characters)."`
	MaxRuns      *int64  `json:"max_runs,omitempty" description:"Stop the loop after this many occurrences (1..10000)."`
	Until        *string `json:"until,omitempty" description:"RFC3339 timestamp with zone; stop the loop before the first occurrence past it."`
}

// WakeListParams is wake_list's input: none (§1.4).
type WakeListParams struct{}

// WakeCancelParams is wake_cancel's input (§1.5).
type WakeCancelParams struct {
	ScheduleID string `json:"schedule_id" description:"The schedule_id returned when the schedule was created."`
}

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
