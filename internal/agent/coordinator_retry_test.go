package agent

// Turn-retry policy tests: shouldRetryStalledMessage, provider-error
// classification (classifyProviderError/isQuotaLimit), turnMadeProgress,
// and shouldRetryTurn.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins the contract of shouldRetryStalledMessage: a watchdog-stalled turn
// is only worth re-running when the assistant message is genuinely empty.
// ANY content reaching the assistant — text, reasoning, even a half-emitted
// tool call — proves the server received and processed the prompt; the
// retry is for cases where nothing came back at all. Prevents the
// duplicate-user-message bug observed in or-coin sessions where z.ai went
// silent at the tail of the stream after a complete reply and the retry
// loop re-sent the same prompt 2× more, copying the user message in the
// DB three times.
func TestShouldRetryStalledMessage(t *testing.T) {
	t.Parallel()

	stalledFinish := message.Finish{
		Reason:  message.FinishReasonError,
		Message: streamStalledFinishTitle,
	}

	t.Run("no finish part returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant}
		assert.False(t, shouldRetryStalledMessage(m))
	})

	t.Run("non-stalled finish returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.Finish{Reason: message.FinishReasonEndTurn},
		}}
		assert.False(t, shouldRetryStalledMessage(m))
	})

	t.Run("stalled with no content returns true", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{stalledFinish}}
		assert.True(t, shouldRetryStalledMessage(m), "empty stalled turn must be retried")
	})

	t.Run("stalled with whitespace-only text returns true", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "   \n\t "},
			stalledFinish,
		}}
		assert.True(t, shouldRetryStalledMessage(m), "whitespace-only output is no output")
	})

	t.Run("stalled with any real text returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "ok"},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "any answer means the server saw the prompt")
	})

	t.Run("stalled with reasoning only returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "considering options..."},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "reasoning proves the model started working")
	})

	t.Run("stalled with finished tool call returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash", Finished: true},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "a completed tool call counts as real work")
	})

	t.Run("stalled with unfinished tool call returns false", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash", Finished: false},
			stalledFinish,
		}}
		assert.False(t, shouldRetryStalledMessage(m), "even a partial tool call proves the prompt was received")
	})
}

// providerErr is a tiny constructor to keep the classify table terse.
func providerErr(status int, msg string) *fantasy.ProviderError {
	return &fantasy.ProviderError{
		StatusCode: status,
		Title:      fantasy.ErrorTitleForStatusCode(status),
		Message:    msg,
	}
}

func TestClassifyProviderError(t *testing.T) {
	t.Parallel()

	quotaMsg := "Usage limit reached for 5 hour. Your limit will reset at 2025-01-01T00:00:00Z"
	overloadMsg := "The service may be temporarily overloaded"

	// status 0 wrapping io.ErrUnexpectedEOF → IsRetryable()==true.
	zeroRetryable := &fantasy.ProviderError{
		StatusCode: 0,
		Message:    io.ErrUnexpectedEOF.Error(),
		Cause:      io.ErrUnexpectedEOF,
	}
	// status 0 with a non-retryable cause → terminal.
	zeroTerminal := &fantasy.ProviderError{StatusCode: 0, Message: "weird"}

	// context-too-large: IsContextTooLarge() reads ContextMaxTokens / ContextTooLargeErr.
	contextTooLarge := &fantasy.ProviderError{StatusCode: 400, ContextMaxTokens: 200000}

	tests := []struct {
		name string
		err  error
		want retryClass
	}{
		{"context.Canceled", context.Canceled, classTerminal},
		{"context.DeadlineExceeded", context.DeadlineExceeded, classTerminal},

		{"401", providerErr(http.StatusUnauthorized, "nope"), classTerminal},
		{"402", providerErr(http.StatusPaymentRequired, "pay"), classTerminal},
		{"403", providerErr(http.StatusForbidden, "forbidden"), classTransient},

		{"429 quota wall", providerErr(http.StatusTooManyRequests, quotaMsg), classTerminal},
		{"429 overload", providerErr(http.StatusTooManyRequests, overloadMsg), classTransient},

		{"408", providerErr(http.StatusRequestTimeout, "timeout"), classTransient},
		{"409", providerErr(http.StatusConflict, "conflict"), classTransient},

		{"500", providerErr(http.StatusInternalServerError, "boom"), classTransient},
		{"503", providerErr(http.StatusServiceUnavailable, "down"), classTransient},

		{"400", providerErr(http.StatusBadRequest, "bad"), classTerminal},
		{"404", providerErr(http.StatusNotFound, "missing"), classTerminal},

		{"status 0 EOF retryable", zeroRetryable, classTransient},
		{"status 0 non-retryable", zeroTerminal, classTerminal},

		{"context-too-large", contextTooLarge, classTerminal},

		{"plain net.OpError (no ProviderError)", &net.OpError{Op: "read", Err: errors.New("connection reset")}, classTransient},
		{"plain generic error", errors.New("something else"), classTerminal},

		// RetryError wrapping must be transparent to errors.As.
		{
			"RetryError wrapping 429 overload",
			&fantasy.RetryError{Errors: []error{providerErr(http.StatusTooManyRequests, overloadMsg)}},
			classTransient,
		},
		{
			"RetryError wrapping 429 quota",
			&fantasy.RetryError{Errors: []error{providerErr(http.StatusTooManyRequests, quotaMsg)}},
			classTerminal,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyProviderError(tc.err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestIsQuotaLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *fantasy.ProviderError
		want bool
	}{
		{"5h usage limit", providerErr(http.StatusTooManyRequests, "Usage limit reached for 5 hour. Your limit will reset at 2025-01-01T00:00:00Z"), true},
		{"reset at", providerErr(http.StatusTooManyRequests, "Rate limit reset at epoch 1234"), true},
		{"quota", providerErr(http.StatusTooManyRequests, "You exceeded your quota"), true},
		{"overload", providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded"), false},
		{"empty message", providerErr(http.StatusTooManyRequests, ""), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isQuotaLimit(tc.err))
		})
	}
}

func TestTurnMadeProgress(t *testing.T) {
	t.Parallel()

	t.Run("empty message is no progress", func(t *testing.T) {
		t.Parallel()
		assert.False(t, turnMadeProgress(message.Message{Role: message.Assistant}))
	})
	t.Run("whitespace only is no progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "  \n\t "},
		}}
		assert.False(t, turnMadeProgress(m))
	})
	t.Run("text is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		}}
		assert.True(t, turnMadeProgress(m))
	})
	t.Run("reasoning is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking..."},
		}}
		assert.True(t, turnMadeProgress(m))
	})
	t.Run("tool call is progress", func(t *testing.T) {
		t.Parallel()
		m := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "1", Name: "bash"},
		}}
		assert.True(t, turnMadeProgress(m))
	})
}

// appendAssistant finishes a fresh assistant message in the session with
// the given parts, returning the coordinator bound to the test's message
// service so shouldRetryTurn can read it back.
func appendAssistant(t *testing.T, env fakeEnv, parts []message.ContentPart) (*coordinator, string) {
	t.Helper()
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}

	sess, err := env.sessions.Create(t.Context(), "retry-test")
	require.NoError(t, err)

	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: parts,
	})
	require.NoError(t, err)
	return coord, sess.ID
}

func TestShouldRetryTurn(t *testing.T) {
	emptyStreamFinish := message.Finish{Reason: message.FinishReasonError, Message: "Empty response"}
	stallFinish := message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle}
	cleanFinish := message.Finish{Reason: message.FinishReasonEndTurn}

	overloadErr := providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded")
	quotaErr := providerErr(http.StatusTooManyRequests, "Your quota has been exhausted")

	t.Run("stall title with nil error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{stallFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, context.Canceled, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("stall title does not retry when the call carries a positive IdleTimeout", func(t *testing.T) {
		// `rush run --idle-timeout` (CallOptions.IdleTimeout > 0) must end
		// the run outright instead of silently absorbing
		// streamStallRetriesDefault extra attempts — see IdleTimeout's doc.
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{stallFinish})
		ctx := WithCallOptions(t.Context(), &CallOptions{IdleTimeout: 15 * time.Minute})
		assert.False(t, coord.shouldRetryTurn(ctx, sid, context.Canceled, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("empty-stream finish with nil error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, nil, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("429 overload error retries", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.True(t, coord.shouldRetryTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("429 quota error does not retry", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{emptyStreamFinish})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, quotaErr, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("turn with content does not retry even on transient error", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			emptyStreamFinish,
		})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("clean end_turn finish does not retry", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{cleanFinish})
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, nil, "" /* Fresh session: no pre-attempt assistant rows. */))
	})

	t.Run("no assistant message does not retry", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
		sess, err := env.sessions.Create(t.Context(), "empty")
		require.NoError(t, err)
		assert.False(t, coord.shouldRetryTurn(t.Context(), sess.ID, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */))
	})
}

func TestShouldContinueTurn(t *testing.T) {
	stallFinish := message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle}
	overloadFinish := message.Finish{Reason: message.FinishReasonError, Message: "Empty response"}
	cleanFinish := message.Finish{Reason: message.FinishReasonEndTurn}

	overloadErr := providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded")
	quotaErr := providerErr(http.StatusTooManyRequests, "Your quota has been exhausted")

	t.Run("stall with partial text continues", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			stallFinish,
		})
		msg, ok := coord.shouldContinueTurn(t.Context(), sid, context.Canceled, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.True(t, ok)
		assert.Equal(t, "partial answer", msg.FullText())
	})

	t.Run("stall with partial text does not continue when the call carries a positive IdleTimeout", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			stallFinish,
		})
		ctx := WithCallOptions(t.Context(), &CallOptions{IdleTimeout: 15 * time.Minute})
		_, ok := coord.shouldContinueTurn(ctx, sid, context.Canceled, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok, "an --idle-timeout-armed call must end the run, not resume via a continuation prompt")
	})

	t.Run("transient error with partial text continues", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			overloadFinish,
		})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.True(t, ok)
	})

	t.Run("transient error with partial reasoning only continues", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.ReasoningContent{Thinking: "considering options..."},
			overloadFinish,
		})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.True(t, ok)
	})

	t.Run("terminal (quota) error with partial text does not continue", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			overloadFinish,
		})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, quotaErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok)
	})

	t.Run("no progress does not continue (blind-retry path owns it)", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{overloadFinish})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok)
	})

	t.Run("nil error with progress does not continue", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "partial answer"},
			overloadFinish,
		})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, nil, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok)
	})

	t.Run("clean finish does not continue", func(t *testing.T) {
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.TextContent{Text: "done"},
			cleanFinish,
		})
		_, ok := coord.shouldContinueTurn(t.Context(), sid, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok)
	})

	t.Run("no assistant message does not continue", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
		sess, err := env.sessions.Create(t.Context(), "empty")
		require.NoError(t, err)
		_, ok := coord.shouldContinueTurn(t.Context(), sess.ID, overloadErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok)
	})
}

func TestContinuationPrompt(t *testing.T) {
	t.Run("quotes partial text and asks to continue", func(t *testing.T) {
		partial := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "here is the start of my answer"},
		}}
		got := continuationPrompt("do the thing", partial)
		assert.Contains(t, got, "do the thing")
		assert.Contains(t, got, "here is the start of my answer")
		assert.Contains(t, got, "Continue")
	})

	t.Run("no partial text falls back to naming the interruption", func(t *testing.T) {
		partial := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking..."},
		}}
		got := continuationPrompt("do the thing", partial)
		assert.Contains(t, got, "do the thing")
		assert.NotContains(t, got, "thinking...")
	})
}

// TestRunInternal_ContinuationRetry_PreservesPartialContent reproduces the
// real-world incident this fix targets: a watchdog stall (or rate limit)
// fires AFTER the model has already streamed partial content. Before this
// fix, shouldRetryTurn's turnMadeProgress guard made the turn fail
// terminally the instant any content existed -- exactly the case that
// matters in practice, since a 3-minute stream-stall watchdog almost always
// fires mid-answer, not before the first token. This test asserts the turn
// now continues instead of dying, and that the partial assistant message is
// left untouched in history (not overwritten, not duplicated).
func TestRunInternal_ContinuationRetry_PreservesPartialContent(t *testing.T) {
	const providerID = "test-continuation-retry"
	const prompt = "write a long report"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "continuation-retry-test")
	require.NoError(t, err)

	callCount := 0
	var secondCallPrompt string
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		if callCount == 1 {
			require.Empty(t, call.ExistingMessageID, "first attempt must create its own user message")
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)

			// The model wrote a partial answer before a transient watchdog
			// stall cut the stream -- this is the case the fix targets.
			_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.TextContent{Text: "Section 1: Introduction. Here is the beginning of the report..."},
					message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
				},
			})
			require.NoError(t, err)
			return nil, context.Canceled // watchdog stalls surface as context.Canceled
		}
		// Continuation attempt: must NOT reuse the original user message
		// (this is a fresh follow-up turn, not an edit), and its prompt must
		// reference the partial content instead of blindly repeating the
		// original prompt.
		secondCallPrompt = call.Prompt
		assert.Empty(t, call.ExistingMessageID, "continuation must create a fresh user message, not overwrite the original")
		_, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Section 2: Conclusion."},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		return agentResultWithText("Section 2: Conclusion."), nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, callCount, "exactly one continuation attempt expected")

	assert.Contains(t, secondCallPrompt, prompt, "continuation prompt must reference the original request")
	assert.Contains(t, secondCallPrompt, "Section 1: Introduction", "continuation prompt must quote the partial content")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var assistantTexts []string
	for _, m := range msgs {
		if m.Role == message.Assistant {
			assistantTexts = append(assistantTexts, m.FullText())
		}
	}
	require.Len(t, assistantTexts, 2, "the partial assistant message must be preserved, not overwritten")
	assert.Contains(t, assistantTexts[0], "Section 1: Introduction", "the original partial answer must survive untouched")
	assert.Contains(t, assistantTexts[1], "Section 2: Conclusion")
}

// TestRetryClassifiers_AttemptScopedEvidence pins the R3-1 fix: the retry
// classifiers must only act on evidence the CURRENT call's own attempt
// produced. Two independent guards enforce this — an admission-shaped
// refusal (ErrSessionBusy, ErrAgentShuttingDown, OS session-lock busy)
// short-circuits classification entirely, and an assistant row whose ID
// matches the pre-attempt baseline is someone else's (or an earlier
// attempt's) evidence and never authorizes a retry or continuation.
// Without these guards, caller A refused at admission would observe
// caller B's stalled partial assistant message as the session's last row
// and "resume" it with a continuation prompt — an unauthorized retry
// execution quoting text A's call never produced.
func TestRetryClassifiers_AttemptScopedEvidence(t *testing.T) {
	t.Parallel()
	partialStalledParts := func(text string) []message.ContentPart {
		return []message.ContentPart{
			message.TextContent{Text: text},
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		}
	}

	t.Run("admission refusal with foreign stalled partial does not continue", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		// Caller B's stalled partial is the session's last assistant row.
		coord, sid := appendAssistant(t, env, partialStalledParts("partial answer"))
		// Mirrors agent_run.go's pre-attempt refusal exactly (including
		// the %w wrap the fail-fast caller sees). The refusal guard must
		// fire regardless of the baseline value, hence "".
		busyErr := fmt.Errorf("session %q is already processing another request: %w", sid, ErrSessionBusy)
		_, ok := coord.shouldContinueTurn(t.Context(), sid, busyErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok, "a refused call must not resume a foreign stalled message as a continuation")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, busyErr, "" /* Fresh session: no pre-attempt assistant rows. */), "a refused call must not be retried")
	})

	t.Run("shutdown refusal does not retry or continue", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, partialStalledParts("partial answer"))
		_, ok := coord.shouldContinueTurn(t.Context(), sid, ErrAgentShuttingDown, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok, "a shutdown refusal means no attempt ran — nothing to resume")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, ErrAgentShuttingDown, "" /* Fresh session: no pre-attempt assistant rows. */), "a shutdown refusal must not be retried")
	})

	t.Run("own stalled attempt still continues", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions, messages: env.messages}
		sess, err := env.sessions.Create(t.Context(), "own-attempt-evidence")
		require.NoError(t, err)
		// Baseline BEFORE this call's own attempt commits its row.
		baseline := coord.lastAssistantMessageID(t.Context(), sess.ID)
		appended, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: partialStalledParts("partial answer"),
		})
		require.NoError(t, err)
		msg, ok := coord.shouldContinueTurn(t.Context(), sess.ID, context.Canceled, baseline)
		assert.True(t, ok, "a stall of this call's own partial attempt still resumes via a continuation")
		assert.Equal(t, "partial answer", msg.FullText())
		assert.Equal(t, appended.ID, msg.ID)
	})

	t.Run("pre-existing error row with no new evidence does not retry", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		// A stalled no-progress row from an earlier turn is the session's
		// last assistant row; the baseline pins it as NOT this attempt's.
		coord, sid := appendAssistant(t, env, []message.ContentPart{
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		})
		baseline := coord.lastAssistantMessageID(t.Context(), sid)
		require.NotEmpty(t, baseline)
		transientErr := providerErr(http.StatusTooManyRequests, "The service may be temporarily overloaded")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, transientErr, baseline), "a transient error with no new evidence must not resurrect an earlier turn as a retry")
		_, ok := coord.shouldContinueTurn(t.Context(), sid, transientErr, baseline)
		assert.False(t, ok, "a transient error with no new evidence must not continue an earlier turn either")
	})

	t.Run("os-lock busy refusal does not retry", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t)
		coord, sid := appendAssistant(t, env, partialStalledParts("partial answer"))
		// Constructed per internal/session/lock.go's SessionLockBusyError
		// shape; runOwned wraps it as `session %q is already in use: %w`.
		lockBusy := &session.SessionLockBusyError{Path: "/data/locks/" + sid, HolderPID: 424242}
		lockErr := fmt.Errorf("session %q is already in use: %w", sid, lockBusy)
		_, ok := coord.shouldContinueTurn(t.Context(), sid, lockErr, "" /* Fresh session: no pre-attempt assistant rows. */)
		assert.False(t, ok, "an OS session-lock refusal means no attempt ran — nothing to resume")
		assert.False(t, coord.shouldRetryTurn(t.Context(), sid, lockErr, "" /* Fresh session: no pre-attempt assistant rows. */), "an OS session-lock refusal must not be retried")
	})
}

// TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage
// reproduces the R3-1 bug end-to-end at the coordinator level: caller A is
// refused at mailbox admission (FailIfSessionBusy, the exact error
// agent_run.go produces BEFORE any provider request), while caller B's
// stalled partial assistant message is the session's last row. Before the
// fix, the retry loop's shouldContinueTurn saw B's row, decided A's failed
// call had partial progress worth resuming, and re-ran A's call as a
// continuation quoting B's text — an unauthorized retry execution. The
// refusal must surface untouched: exactly one provider call, the original
// prompt, B's message preserved, no retry attempt after the backoff.
// Deliberate scope: a true ExecuteRun-level test needs internal/app files
// owned by a sibling task; runInternal + the mock agent IS the
// coordinator's own retry machinery, and the mailbox refusal itself is
// covered by existing fail-fast tests.
func TestRunInternal_NoRetryAfterAdmissionRefusal_WithForeignStalledMessage(t *testing.T) {
	const providerID = "test-busy-refusal"
	const prompt = "A's reviewer prompt"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "busy-refusal-test")
	require.NoError(t, err)

	// Seed caller B's history: B's stalled partial is the session's last
	// assistant row before A's fail-fast call ever starts.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "review this"}},
	})
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "B's partial answer, cut off"},
			message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle},
		},
	})
	require.NoError(t, err)

	callCount := 0
	var recordedPrompt string
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		recordedPrompt = call.Prompt
		// Fail-fast caller A: exactly the pre-attempt refusal
		// agent_run.go produces for FailIfSessionBusy calls.
		return nil, fmt.Errorf("session %q is already processing another request: %w", call.SessionID, ErrSessionBusy)
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionBusy, "the fail-fast refusal must surface, not be swallowed by the retry machinery")
	assert.Nil(t, res)
	assert.Equal(t, 1, callCount, "no second provider call may happen — not immediately, and no retry exists to run after B releases")
	assert.Equal(t, prompt, recordedPrompt, "the prompt must never be rewritten into a continuation quoting B's text")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var lastAssistantText string
	for _, m := range msgs {
		if m.Role == message.Assistant {
			lastAssistantText = m.FullText()
		}
	}
	assert.Contains(t, lastAssistantText, "B's partial answer", "caller B's stalled message must remain the last assistant row, untouched")
}

// TestRunInternal_RetryReusesUserMessage_NoDuplicate reproduces a real
// incident: a session where the same prompt appeared multiple times in
// history after transient provider failures. Each retry created a fresh
// user message instead of reusing the one the first attempt already
// created. Fix: capture the first attempt's created message ID via
// OnUserMessageCreated and feed it back as ExistingMessageID on retries.
func TestRunInternal_RetryReusesUserMessage_NoDuplicate(t *testing.T) {
	const providerID = "test-retry-dup"
	const prompt = "investigate the flaky provider"

	orig := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { streamStallRetryBaseBackoff = orig })

	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	providerCfg := config.ProviderConfig{
		ID:   providerID,
		Type: "openai",
		Models: []catwalk.Model{
			{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096},
		},
	}
	cfg.Config().Providers.Set(providerID, providerCfg)
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel

	coord := &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		messages:   env.messages,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}

	sess, err := env.sessions.Create(t.Context(), "retry-dup-test")
	require.NoError(t, err)

	var createdUserMsgID string
	callCount := 0
	agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		callCount++
		if callCount == 1 {
			require.Empty(t, call.ExistingMessageID, "first attempt must create its own user message")
			userMsg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
			createdUserMsgID = userMsg.ID
			require.NotNil(t, call.OnUserMessageCreated)
			call.OnUserMessageCreated(userMsg.ID)

			_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.Finish{Reason: message.FinishReasonError, Message: "Empty response"},
				},
			})
			require.NoError(t, err)
			return nil, nil // "turn returned no error" -- shouldRetryTurn's empty-stream retry path
		}
		// Retry: must reuse the message the first attempt created, not a new one.
		assert.Equal(t, createdUserMsgID, call.ExistingMessageID, "retry must reuse the first attempt's user message")
		// A real runTurn would persist a clean assistant finish here; without
		// it, shouldRetryTurn keeps seeing call 1's FinishReasonError message
		// and retries again.
		_, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "recovered"},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
		require.NoError(t, err)
		return agentResultWithText("recovered"), nil
	})
	coord.currentAgent = agent

	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)

	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 2, callCount, "exactly one retry expected")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var userCount int
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == prompt {
			userCount++
		}
	}
	assert.Equal(t, 1, userCount, "the prompt must appear exactly once in history, not once per retry attempt")
}
