package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

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
