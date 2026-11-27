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
