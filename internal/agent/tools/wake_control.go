// Wake-schedule control tools (wakein/wakeon/loop/wake_list/wake_cancel,
// docs/plans/2026-09-27-wake-tools-contract.md §1). Like AgentControl, the
// tool layer is thin: it reads the caller session from context and delegates
// to WakeControl, implemented by the coordinator (coordinator_wake_tools.go)
// over the stage-4a WakeScheduleStore and the stage-4b worker's timer.
package tools

import "context"

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
