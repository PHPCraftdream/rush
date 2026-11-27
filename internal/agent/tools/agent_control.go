// Sub-agent control tools (inspect_agent/inject_agent/stop_agent,
// docs/plans/2026-09-27-wake-tools-contract.md §4). The tool layer is thin:
// it reads the caller session from context and delegates to AgentControl,
// implemented by the agent package's coordinator (see
// coordinator_agent_control.go) so every ownership check, status signal and
// cancel/inject route through the same coordinator paths the rest of the
// async machinery uses.
package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

// AgentControl is the coordinator-side half of the sub-agent control tools.
// Every returned error's message is model-safe (returned via
// NewTextErrorResponse, not a fatal Go error).
type AgentControl interface {
	// InspectAgent reports a delegated child's status (§4.1).
	InspectAgent(ctx context.Context, callerSessionID, childSessionID string) (AgentInspection, error)
	// ListDelegations lists the caller session's live delegations (§4.1's
	// no-argument inspect_agent listing). A delivered delegation is gone
	// from the ledger by design and is not listed.
	ListDelegations(ctx context.Context, callerSessionID string) ([]AgentDelegationSummary, error)
	// InjectAgent delivers msg into the child's session (§4.2) and returns
	// the tool's own answer text.
	InjectAgent(ctx context.Context, callerSessionID, childSessionID, msg string, interrupt bool) (string, error)
	// StopAgent stops the child's current turn and armed delegation (§4.3)
	// and returns the tool's own answer text.
	StopAgent(ctx context.Context, callerSessionID, childSessionID string) (string, error)
}

// AgentInspection is inspect_agent's JSON output (§4.1).
type AgentInspection struct {
	ChildSessionID      string `json:"child_session_id"`
	Status              string `json:"status"`
	QueuedMessages      int    `json:"queued_messages"`
	LastActivityAt      string `json:"last_activity_at,omitempty"`
	LastActivitySummary string `json:"last_activity_summary,omitempty"`
	// HeldQuestion is set when Status is "awaiting_answer" because a
	// CURRENT batch question of this child is still actionable (the fenced
	// marker identity of the held ask); empty for the legacy between-turns
	// question stop.
	HeldQuestion *AgentHeldQuestion `json:"held_question,omitempty"`
}

// AgentHeldQuestion is the fenced identity of a child's held batch
// question: inspect_agent reports it when the child is parked inside a
// live ask_question of a CURRENT batch incarnation.
type AgentHeldQuestion struct {
	NodeID   string `json:"node_id"`
	Token    int64  `json:"token"`
	Depth    int    `json:"depth"`
	Sequence int64  `json:"sequence"`
}

// AgentDelegationSummary is one live delegation in ListDelegations' output.
type AgentDelegationSummary struct {
	ChildSessionID string `json:"child_session_id"`
	ToolName       string `json:"tool_name"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at,omitempty"`
	AgeSeconds     int    `json:"age_seconds,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

// childSessionIDParam is the shared ownership-checked parameter.
type childSessionIDParam struct {
	ChildSessionID string `json:"child_session_id" description:"The delegated sub-agent's session id (the id the agent tool reported when the delegation started)."`
}

func childSessionFromParams(params childSessionIDParam) (string, error) {
	if params.ChildSessionID == "" {
		return "", fmt.Errorf("child_session_id is required: pass the id the agent tool reported for the delegation, or call inspect_agent with no arguments to list your live delegations")
	}
	return params.ChildSessionID, nil
}

// AskQuestionToolName is the tool name the model calls to stop the current
// turn and ask the operator/orchestrator a question.
const AskQuestionToolName = "ask_question"

//go:embed ask_question.md
var askQuestionDescription string

// AskQuestionParams is the JSON schema for the ask_question tool.
type AskQuestionParams struct {
	Question string   `json:"question" description:"The question to ask the operator/orchestrator. Be specific and self-contained: the turn ends immediately after this call, so whoever resumes the session only sees this text, not the preceding conversation."`
	Options  []string `json:"options,omitempty" description:"Optional suggested answers, e.g. [\"yes\", \"no\", \"dry-run only\"]. Advisory only — any free-text answer is still accepted when the session resumes."`
}

// ownRunningJobsContextKey is the context-key type for the session's live
// own-jobs reader.
type ownRunningJobsContextKey string

// OwnRunningJobsContextKey is the context key for the session's live
// own-jobs reader (see RunningJobsReader).
const OwnRunningJobsContextKey ownRunningJobsContextKey = "own_running_jobs"

// RunningJobsReader reports how many of the session's OWN async jobs are
// currently running. ok=false means the fact is unavailable (no reader
// wired, or the read failed) — the tool then keeps the legacy force-finish
// behavior rather than guessing.
type RunningJobsReader func(ctx context.Context, sessionID string) (running int, ok bool)

// WithOwnRunningJobsReader attaches the own-running-jobs reader to the
// turn context.
func WithOwnRunningJobsReader(ctx context.Context, r RunningJobsReader) context.Context {
	return context.WithValue(ctx, OwnRunningJobsContextKey, r)
}

// OwnRunningJobsFromContext reads the session's running own-jobs count
// through the context-carried reader. ok=false when no reader is wired.
func OwnRunningJobsFromContext(ctx context.Context) (running int, ok bool) {
	r, _ := ctx.Value(OwnRunningJobsContextKey).(RunningJobsReader)
	if r == nil {
		return 0, false
	}
	return r(ctx, GetSessionFromContext(ctx))
}

// AskQuestionError is the error the ask_question tool's Run returns to force
// fantasy's agent loop to stop the current turn instead of continuing it
// with a normal (successful) tool result.
//
// This type intentionally lives in package tools, not package agent: package
// agent already imports package tools (see coordinator.go's tool wiring), so
// package tools importing package agent back — to reuse agent.
// AwaitingAnswerError directly — would create an import cycle. agent.Run
// normalizes an AskQuestionError it sees coming back from fantasy's Stream()
// into the pre-existing agent.AwaitingAnswerError (see the errors.As check
// next to the peak-hours abort-err normalization in agent.go), so every
// downstream consumer (Finish-part text, `rush run --json`'s exit_reason,
// sessions why/diff, …) only ever has to know about the one agent-level
// type. This struct is the narrow carrier that crosses the package
// boundary.
type AskQuestionError struct {
	Question  string
	Options   []string
	SessionID string
}

func (e *AskQuestionError) Error() string {
	return fmt.Sprintf("agent asked a question and is awaiting an answer (session %s): %s", e.SessionID, e.Question)
}

// NewAskQuestionTool builds the ask_question agent tool. For a well-formed
// question asked while the session has NO running own async jobs, Run never
// returns a normal (successful) ToolResponse: it returns a non-nil
// *AskQuestionError as the Go error, which fantasy's
// executeSingleTool treats as a critical error and propagates all the way up
// as the agent loop's own error (see charm.land/fantasy's agent.go
// executeSingleTool: a non-nil error from a tool's Run aborts the step and
// is returned from Stream/Run verbatim). That is what lets agent.Run's
// error-classification chain (agent_turn.go) catch it and force-finish the
// turn via AddFinish, exactly like the existing PeakHoursError path.
//
// When the session DOES have running own async jobs, asking would end the
// whole run and orphan that work: Run instead returns an ordinary hint
// response (nil Go error) and the turn continues, so the model can finish
// the turn without tool calls and let the jobs' results arrive as messages.
func NewAskQuestionTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		AskQuestionToolName,
		askQuestionDescription,
		func(ctx context.Context, params AskQuestionParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			question := strings.TrimSpace(params.Question)
			if question == "" {
				// A malformed call (empty question) is a normal, retryable
				// tool error — NOT a real question — so it must NOT stop
				// the turn. Return it as an ordinary error ToolResponse
				// (nil Go error) so fantasy just reports it to the model
				// and lets the turn continue.
				return fantasy.NewTextErrorResponse("question must not be empty"), nil
			}

			sessionID := GetSessionFromContext(ctx)

			if running, ok := OwnRunningJobsFromContext(ctx); ok && running > 0 {
				// The agent has live work in flight (its own background jobs
				// and/or running delegations): ending the run now would
				// orphan it. Do NOT stop the turn — hand the model a hint
				// it can act on and let the turn continue.
				return fantasy.NewTextResponse(fmt.Sprintf(
					"You still have %d running background task(s) (your own jobs and/or live delegations). ask_question does NOT wait for them — calling it would end the whole run and leave them orphaned. Call await_tasks instead to sleep until they finish (until: \"all\" when you need every result), or finish this turn WITHOUT any tool call and let their results arrive as new messages. Reserve ask_question for a decision you cannot make yourself.", running)), nil
			}

			return fantasy.ToolResponse{}, &AskQuestionError{
				Question:  question,
				Options:   params.Options,
				SessionID: sessionID,
			}
		},
	)
}

const InjectAgentToolName = "inject_agent"

//go:embed inject_agent.md
var injectAgentDescription string

type InjectAgentParams struct {
	ChildSessionID string `json:"child_session_id" description:"Full delegated child session id, including any $$call_ suffix. Take it from the child_question notice or inspect_agent listing."`
	Message        string `json:"message" description:"The message to deliver into the sub-agent's session."`
	Interrupt      bool   `json:"interrupt,omitempty" description:"Cancel the sub-agent's current turn and hand it this message immediately instead of waiting for the turn to end. Default false."`
}

// NewInjectAgentTool builds inject_agent (contract §4.2).
func NewInjectAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		InjectAgentToolName,
		injectAgentDescription,
		func(ctx context.Context, params InjectAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to inject into a sub-agent"), nil
			}
			childSessionID, err := childSessionFromParams(childSessionIDParam{ChildSessionID: params.ChildSessionID})
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if strings.TrimSpace(params.Message) == "" {
				return fantasy.NewTextErrorResponse("message is required"), nil
			}
			text, err := control.InjectAgent(ctx, sessionID, childSessionID, params.Message, params.Interrupt)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(text), nil
		},
	)
}

const InspectAgentToolName = "inspect_agent"

//go:embed inspect_agent.md
var inspectAgentDescription string

type InspectAgentParams struct {
	ChildSessionID string `json:"child_session_id,omitempty" description:"Full delegated child session id, including any $$call_ suffix. Omit to list live delegations."`
}

// NewInspectAgentTool builds inspect_agent (contract §4.1): a read-only
// status probe over a delegated child session.
func NewInspectAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		InspectAgentToolName,
		inspectAgentDescription,
		func(ctx context.Context, params InspectAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to inspect a sub-agent"), nil
			}
			if params.ChildSessionID == "" {
				delegations, err := control.ListDelegations(ctx, sessionID)
				if err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				switch len(delegations) {
				case 0:
					return fantasy.NewTextResponse("No live sub-agent delegations for this session. Nothing to inspect; start one with the agent tool."), nil
				case 1:
					insp, err := control.InspectAgent(ctx, sessionID, delegations[0].ChildSessionID)
					if err != nil {
						return fantasy.NewTextErrorResponse(err.Error()), nil
					}
					data, err := json.Marshal(insp)
					if err != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent output: %w", err)
					}
					return fantasy.NewTextResponse(string(data)), nil
				default:
					data, err := json.Marshal(delegations)
					if err != nil {
						return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent listing: %w", err)
					}
					return fantasy.NewTextResponse("Multiple live sub-agent delegations; call again with child_session_id from this list:\n" + string(data)), nil
				}
			}
			childSessionID, err := childSessionFromParams(childSessionIDParam(params))
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			insp, err := control.InspectAgent(ctx, sessionID, childSessionID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			data, err := json.Marshal(insp)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("marshal inspect_agent output: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

const StopAgentToolName = "stop_agent"

//go:embed stop_agent.md
var stopAgentDescription string

type StopAgentParams struct {
	ChildSessionID string `json:"child_session_id" description:"Full delegated child session id, including any $$call_ suffix. Take it from the child_question notice or inspect_agent listing."`
}

// NewStopAgentTool builds stop_agent (contract §4.3): stops ONLY the child's
// current turn and armed delegation; history is preserved.
func NewStopAgentTool(control AgentControl) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		StopAgentToolName,
		stopAgentDescription,
		func(ctx context.Context, params StopAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.NewTextErrorResponse("session ID is required to stop a sub-agent"), nil
			}
			childSessionID, err := childSessionFromParams(childSessionIDParam(params))
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			text, err := control.StopAgent(ctx, sessionID, childSessionID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(text), nil
		},
	)
}

// TimeoutParams is the shared explicit per-call timeout shape for
// bash/run_command/agent (docs/plans/2026-09-27-wake-tools-contract.md §3).
// Timeouts are per-call only, never implicit -- no field here defaults to a
// non-zero value. Parsed generically (not via this type) in
// internal/agent's parseTimeoutParam; this type exists so all three tools'
// Params structs describe the identical JSON shape in their schema.
type TimeoutParams struct {
	Seconds int    `json:"seconds" description:"5-604800 (7 days). Seconds from call start."`
	Kind    string `json:"kind" description:"Required when seconds is set: \"wake_only\" or \"terminate_and_wake\"."`
}

// UnmarshalJSON accepts the documented object shape and, for models that
// send a bare number of seconds, also a JSON number -- read as
// {"seconds": N, "kind": "wake_only"}, the safe kind (it never kills
// running work).
func (p *TimeoutParams) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] == 'n' {
		return nil
	}
	if trimmed[0] == '{' {
		type plain TimeoutParams
		return json.Unmarshal(trimmed, (*plain)(p))
	}
	var seconds int
	if err := json.Unmarshal(trimmed, &seconds); err == nil {
		p.Seconds = seconds
		p.Kind = "wake_only"
		return nil
	}
	return fmt.Errorf(`timeout must be an object {"seconds": N, "kind": "wake_only"|"terminate_and_wake"} or a bare number of seconds`)
}

// Wake-schedule control tools (wakein/wakeon/loop/wake_list/wake_cancel,
// docs/plans/2026-09-27-wake-tools-contract.md §1). Like AgentControl, the
// tool layer is thin: it reads the caller session from context and delegates
// to WakeControl, implemented by the coordinator (coordinator_wake_tools.go)
// over the stage-4a WakeScheduleStore and the stage-4b worker's timer.

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
