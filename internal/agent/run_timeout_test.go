package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineExceededModel is a fantasy.LanguageModel whose Stream call fails
// with a bare context.DeadlineExceeded, simulating what `rush run
// --timeout` produces when its root context deadline fires mid-turn (see
// run.go's timeoutDur handling). Only Stream is exercised by Run().
type deadlineExceededModel struct{}

func (deadlineExceededModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, context.DeadlineExceeded
}

func (deadlineExceededModel) Provider() string { return "test" }
func (deadlineExceededModel) Model() string    { return "deadline-exceeded" }

// waitForDeadlineModel is a fantasy.LanguageModel whose Stream waits for the
// caller's context to end and fails with the context's own error: a request
// cut by the run's real `--timeout` deadline.
type waitForDeadlineModel struct{ deadlineExceededModel }

func (waitForDeadlineModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("waitForDeadlineModel: the deadline never arrived")
	}
}

func (waitForDeadlineModel) Model() string { return "wait-for-deadline" }

// TestRun_RunTimeoutDeadlineExceededSurfacesClearFinish reproduces the
// regression found while triaging a crashed session in another repo: a
// context.DeadlineExceeded from the run's own root --timeout wasn't matched
// by isCancelErr (errors.Is(err, context.Canceled) only), so it fell into
// the generic else branch as an unhelpful "Provider Error" indistinguishable
// from a real provider failure. It must now surface as a dedicated "Run
// timeout exceeded" finish.
//
// The turn's own context really is past its deadline here (the model waits for
// it): an error that merely satisfies errors.Is(err, context.DeadlineExceeded)
// with a live turn context is a provider failure, see
// TestRun_TransportDeadlineWithLiveContextIsAProviderError.
func TestRun_RunTimeoutDeadlineExceededSurfacesClearFinish(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, waitForDeadlineModel{}, waitForDeadlineModel{}, "test system prompt")

	// A titled session with history: no title generation (which would also
	// wait on the model) races the deadline.
	sess, err := env.sessions.Create(t.Context(), "Run timeout")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier"}},
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = agent.Run(runCtx, SessionAgentCall{
		Prompt:          "hello",
		SessionID:       sess.ID,
		MaxOutputTokens: 100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	finish := lastAssistantFinish(t, env, sess.ID)
	assert.Equal(t, message.FinishReasonError, finish.Reason)
	assert.Equal(t, "Run timeout exceeded", finish.Message)
}

// TestRun_TransportDeadlineWithLiveContextIsAProviderError: a bare
// context.DeadlineExceeded from the provider call while the turn's own context
// is alive (what a net/http Client.Timeout looks like) is a provider failure,
// not the operator's `--timeout`: the generic provider-error finish, not "Run
// timeout exceeded".
//
// Revert-check: deciding from errors.Is(err, context.DeadlineExceeded) alone
// (handleStreamFailure's isRunTimeout) writes "Run timeout exceeded" and this
// test goes red.
func TestRun_TransportDeadlineWithLiveContextIsAProviderError(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, deadlineExceededModel{}, deadlineExceededModel{}, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       sess.ID,
		MaxOutputTokens: 100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	finish := lastAssistantFinish(t, env, sess.ID)
	assert.Equal(t, message.FinishReasonError, finish.Reason)
	assert.Equal(t, "Provider Error", finish.Message)
}

// lastAssistantFinish is the finish part of the session's newest assistant
// message.
func lastAssistantFinish(t *testing.T, env fakeEnv, sessionID string) *message.Finish {
	t.Helper()
	msgs, err := env.messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "expected an assistant message")
	finish := assistant.FinishPart()
	require.NotNil(t, finish)
	return finish
}
