// A step's reaction is recorded even when the turn ends right after it
// (cap/peak abort), and a question the agent asks is a reaction too
// (docs/reviews/2026-09-30-async-phase4-round2-attempts-design.md sec.2,
// R2B-1 (A)/(B)). Real SQLite, real *sessionAgent, httptest provider.
package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestOnStepFinish_CapAbortStepIsARecordedReaction: a Drain step that pushes
// the session past --max-tokens is still answered and billed, so the notice
// it reacted to must be reacted=1 when the cap aborts the turn.
//
// Revert-check: restoring the old onStepFinish order (caps/peak before
// persistStepFinish) leaves the row reacted=0 and this test red.
func TestOnStepFinish_CapAbortStepIsARecordedReaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "cap-abort-reaction", attemptFixtureOpts{noIdle: true})
	f.seedDebt(ctx, "call-1", false)

	call := newDrainCall(SessionAgentCall{SessionID: f.sessID})
	call.MaxTokens = 1 // the probe reply reports 7 tokens
	_, _ = f.sa.Run(ctx, call)

	require.EqualValues(t, 1, f.requests.Load())
	require.EqualValues(t, 1, f.row(ctx, "call-1").Reacted, "the step that crossed the cap is a recorded reaction")
}

// TestAskQuestion_InDrainTurnIsAReaction: fantasy never calls OnStepFinish for
// a step whose tool returns AskQuestionError, so the error path itself must
// record the question as the reaction to the notice the model just answered.
//
// Revert-check: writing the awaiting finish with a plain messages.Update
// (persistFailureFinish) leaves the row reacted=0 and this test red.
func TestAskQuestion_InDrainTurnIsAReaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "ask-question-reaction", attemptFixtureOpts{
		noIdle:  true,
		tools:   []fantasy.AgentTool{tools.NewAskQuestionTool()},
		handler: askQuestionResponse,
	})
	f.seedDebt(ctx, "call-1", false)

	_, err := f.sa.Run(ctx, newDrainCall(SessionAgentCall{SessionID: f.sessID}))
	var awaiting *AwaitingAnswerError
	require.True(t, errors.As(err, &awaiting), "the turn ends with the question: %v", err)

	require.EqualValues(t, 1, f.row(ctx, "call-1").Reacted, "the question is the reaction to the notice")
	msgs, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var last message.Message
	for _, m := range msgs {
		if m.Role == message.Assistant {
			last = m
		}
	}
	fp := last.FinishPart()
	require.NotNil(t, fp)
	require.True(t, strings.Contains(fp.Message, "asked a question"), "the finish still names the question: %q", fp.Message)
}

// askQuestionResponse serves one step whose only content is an ask_question
// tool call.
func askQuestionResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_q","type":"function","function":{"name":"ask_question","arguments":"{\"question\":\"which one?\"}"}}]},"finish_reason":null}]}`)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl != nil {
		fl.Flush()
	}
}

// textThenAskQuestionResponse streams ONE step that writes a sentence and then
// calls ask_question: the child's question comes with its own preamble text.
func textThenAskQuestionResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"Checked the job output; one detail is unclear."},"finish_reason":null}]}`)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_q","type":"function","function":{"name":"ask_question","arguments":"{\"question\":\"which one?\"}"}}]},"finish_reason":null}]}`)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl != nil {
		fl.Flush()
	}
}
