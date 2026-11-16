package wire

import (
	"encoding/json"
	"time"
)

// ── Live work panel (tasks #1058/#1059) ─────────────────────────────────────
//
// handlers_livework.go builds the snapshot from the durable async_jobs table
// (session.AsyncJobStore.JobsInTree, the read-pool reader), pushes
// EventSessionLiveWork on change events, and answers CmdGetSessionLiveWork
// with the same full snapshot.

// GetSessionLiveWorkPayload requests a live-work snapshot for one session.
type GetSessionLiveWorkPayload struct {
	SessionID string `json:"sessionID"`
}

// LiveWorkItemWire is one async command (bash/run_command) or sub-agent
// delegation (agent/agentic_fetch) row for the live-work panel (task #1058).
// ChildSessionID is set only for a delegation -- the sub-agent's own session
// ID. StartedAt/FinishedAt/LastActivityAt are unix milliseconds; FinishedAt
// is 0 while the row is still running. Status is the durable async_jobs.state
// ("running"|"completed"|"failed"|"timed_out"|"cancelled"|"interrupted") and
// Reason the row's notice_kind for a user-stopped command,
// "session_cancel" for a session-level stop, ...) so the client can tell
// user-stopped apart from timed out apart from failed. Mirrors
// web/src/types.ts's LiveWorkItem.
type LiveWorkItemWire struct {
	ToolCallID     string `json:"toolCallID"`
	ToolName       string `json:"toolName"`
	Title          string `json:"title"`
	ChildSessionID string `json:"childSessionID,omitempty"`
	StartedAt      int64  `json:"startedAt"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	FinishedAt     int64  `json:"finishedAt"`
	LastActivityAt int64  `json:"lastActivityAt"`
	// AwaitingAnswer marks a running delegation whose child paused on a
	// question (#1158): set only together with AwaitingQuestion (the
	// display-capped question text). Additive to the wire shape.
	AwaitingAnswer   bool   `json:"awaitingAnswer,omitempty"`
	AwaitingQuestion string `json:"awaitingQuestion,omitempty"`
}

// SessionLiveWorkPayload is both the session_live_work push and the
// get_session_live_work reply: a full snapshot of one session's live async
// commands and sub-agent delegations, replacing any prior snapshot for this
// SessionID client-side. Never a delta.
type SessionLiveWorkPayload struct {
	SessionID string             `json:"sessionID"`
	Commands  []LiveWorkItemWire `json:"commands"`
	Agents    []LiveWorkItemWire `json:"agents"`
	// R12-C, additive: set ONLY on the batch-runtime ON path
	// (handlers_livework_batch.go). Nil is the flag-off path — no wire bytes
	// change. ON always sets it: true on a full read, false on a partial
	// read. False means the batch-node read was partial — the client must
	// MERGE this snapshot into the previously shown work, never replace (an
	// incomplete read never drops active work). A pointer keeps complete
	//=false visible on the wire: with omitempty, a plain false bool was
	// indistinguishable from the flag-off zero value.
	Complete *bool `json:"complete,omitempty"`
}

// GetSessionBatchWorkPayload requests a structured batch-work snapshot
// (R15a, handlers_batch.go) for one session. Wire shape mirrors
// GetSessionLiveWorkPayload.
type GetSessionBatchWorkPayload struct {
	SessionID string `json:"sessionID"`
}

// ── Wake schedules panel (stage 5b) ─────────────────────────────────────────
//
// handlers_wake_schedules.go builds the snapshot from the durable
// wake_schedules table (session.WakeScheduleStore.ListSchedules, the owner's
// rows only), pushes EventSessionWakeSchedules on change events, answers
// CmdGetSessionWakeSchedules with it, and answers CmdCancelWakeSchedule with
// a fresh snapshot after the cancel commits.

// GetSessionWakeSchedulesPayload requests the wake-schedule snapshot for
// one session.
type GetSessionWakeSchedulesPayload struct {
	SessionID string `json:"sessionID"`
}

// CancelWakeSchedulePayload asks to cancel one schedule owned by SessionID.
// A foreign or unknown ScheduleID is an explicit error, never a silent no-op.
type CancelWakeSchedulePayload struct {
	SessionID  string `json:"sessionID"`
	ScheduleID string `json:"scheduleID"`
}

// WakeScheduleWire is one wake_schedules row for the panel. NextRunAt and
// UntilAt are unix milliseconds (UntilAt 0 = unset); MaxRuns 0 = unbounded.
// Message is truncated to the live-work title budget. Mirrors
// web/src/types.ts's WakeScheduleItem.
type WakeScheduleWire struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	NextRunAt  int64  `json:"nextRunAt"`
	EveryMs    int64  `json:"everyMs"`
	MaxRuns    int64  `json:"maxRuns"`
	UntilAt    int64  `json:"untilAt"`
	State      string `json:"state"`
	Occurrence int64  `json:"occurrence"`
	CreatedAt  int64  `json:"createdAt"`
}

// SessionWakeSchedulesPayload is the session_wake_schedules push and the
// get/cancel reply: a full snapshot of one session's wake schedules,
// replacing any prior snapshot for this SessionID client-side. Never a delta.
type SessionWakeSchedulesPayload struct {
	SessionID string             `json:"sessionID"`
	Schedules []WakeScheduleWire `json:"schedules"`
}

type APIEvent struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at"`
}

type APIRunState string

const (
	APIRunQueued         APIRunState = "queued"
	APIRunRunning        APIRunState = "running"
	APIRunSucceeded      APIRunState = "succeeded"
	APIRunFailed         APIRunState = "failed"
	APIRunCancelled      APIRunState = "cancelled"
	APIRunAwaitingAnswer APIRunState = "awaiting_answer"
)

func TerminalAPIRunState(state APIRunState) bool {
	return state == APIRunSucceeded || state == APIRunFailed || state == APIRunCancelled
}

type HelloPayload struct {
	Protocol string   `json:"protocol"`
	TokenID  string   `json:"token_id"`
	Scopes   []string `json:"scopes"`
}

func MustJSONRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
