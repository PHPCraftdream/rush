package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionAgent is a minimal mock for the SessionAgent interface.
//
// mu guards interruptAndReplaced specifically: it's the only field written
// from a background goroutine (coordinator.startInterruptTicker's ticker,
// via handleInterruptTick -> InterruptAndReplace) while a test goroutine may
// concurrently poll it (e.g. via require.Eventually) before the ticker has
// been joined. Every other field is written synchronously on the test
// goroutine's own call into the mock, so it doesn't need the same guard.
// Callers that read interruptAndReplaced AFTER properly joining the ticker
// (waiting on the <-tickerDone channel, which is itself a happens-before
// edge) may keep reading the field directly; only callers that might race
// with an in-flight ticker should go through interruptAndReplacedSnapshot.
type mockSessionAgent struct {
	model                Model
	runFunc              func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error)
	cancelled            []string
	queuedCalls          []SessionAgentCall
	mu                   sync.Mutex
	interruptAndReplaced []SessionAgentCall
}

// interruptAndReplacedSnapshot returns a thread-safe copy of
// interruptAndReplaced. Use this (not the raw field) from any goroutine that
// might read concurrently with a still-running interrupt ticker.
func (m *mockSessionAgent) interruptAndReplacedSnapshot() []SessionAgentCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionAgentCall, len(m.interruptAndReplaced))
	copy(out, m.interruptAndReplaced)
	return out
}

func (m *mockSessionAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	return m.runFunc(ctx, call)
}

func (m *mockSessionAgent) Model() Model                        { return m.model }
func (m *mockSessionAgent) SetModels(large, small Model)        {}
func (m *mockSessionAgent) SetTools(tools []fantasy.AgentTool)  {}
func (m *mockSessionAgent) SetSystemPrompt(systemPrompt string) {}
func (m *mockSessionAgent) Cancel(sessionID string) {
	m.cancelled = append(m.cancelled, sessionID)
}
func (m *mockSessionAgent) CancelAll() (stillBusy bool)                 { return false }
func (m *mockSessionAgent) IsSessionBusy(sessionID string) bool         { return false }
func (m *mockSessionAgent) IsBusy() bool                                { return false }
func (m *mockSessionAgent) QueuedPrompts(sessionID string) int          { return 0 }
func (m *mockSessionAgent) QueuedPromptsList(sessionID string) []string { return nil }
func (m *mockSessionAgent) ClearQueue(sessionID string)                 {}
func (m *mockSessionAgent) QueueMessage(call SessionAgentCall) {
	m.queuedCalls = append(m.queuedCalls, call)
}

func (m *mockSessionAgent) InterruptAndReplace(_ string, call SessionAgentCall) bool {
	m.mu.Lock()
	m.interruptAndReplaced = append(m.interruptAndReplaced, call)
	m.mu.Unlock()
	return true
}

func (m *mockSessionAgent) InjectMessage(_ context.Context, call SessionAgentCall) (message.Message, error) {
	m.queuedCalls = append(m.queuedCalls, call)
	return message.Message{SessionID: call.SessionID}, nil
}

func (m *mockSessionAgent) Summarize(context.Context, string, *SummarizeSnapshot) error {
	return nil
}
func (m *mockSessionAgent) SummarizeQueued(string) bool { return false }
func (m *mockSessionAgent) TakeSummarizeQueue(string) (*SummarizeSnapshot, bool) {
	return nil, false
}
func (m *mockSessionAgent) CancelQueuedSummarize(string)          {}
func (m *mockSessionAgent) SetSystemPromptPrefix(string)          {}
func (m *mockSessionAgent) SystemPrompt() string                  { return "" }
func (m *mockSessionAgent) SetTimeoutOptions(bool, time.Duration) {}

// newTestCoordinator creates a minimal coordinator for unit testing runSubAgent.
//
// Registers providerCfg under providerID AND wires it as both the large and
// small model default (config.SelectedModelType{Large,Small}), so any test
// path that reaches resolveSessionModels (e.g. InterruptAndSend/Run without
// explicit overrides) resolves successfully instead of falling through to
// config.Load's own CLI-provider auto-discovery default — which depends on
// what's actually installed on the machine running the test (a dev
// workstation with claude/gemini CLIs on PATH resolves a real provider
// there; a clean CI sandbox does not), and, in a minimal test coordinator
// that doesn't wire c.permissions, that path panics on a nil dereference.
// providerCfg's first configured Model ID (if any) is used; callers that
// pass a providerCfg with no Models can still use this coordinator for
// tests that never reach resolveSessionModels.
func newTestCoordinator(t *testing.T, env fakeEnv, providerID string, providerCfg config.ProviderConfig) *coordinator {
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, providerCfg)
	if len(providerCfg.Models) > 0 {
		selected := config.SelectedModel{Provider: providerID, Model: providerCfg.Models[0].ID}
		cfg.Config().Models[config.SelectedModelTypeLarge] = selected
		cfg.Config().Models[config.SelectedModelTypeSmall] = selected
	}
	return &coordinator{
		cfg:        cfg,
		sessions:   env.sessions,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
}

// newMockAgent creates a mockSessionAgent with the given provider and run function.
func newMockAgent(providerID string, maxTokens int64, runFunc func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error)) *mockSessionAgent {
	return &mockSessionAgent{
		model: Model{
			CatwalkCfg: catwalk.Model{
				DefaultMaxTokens: maxTokens,
			},
			ModelCfg: config.SelectedModel{
				Provider: providerID,
			},
		},
		runFunc: runFunc,
	}
}

// agentResultWithText creates a minimal AgentResult with the given text response.
func agentResultWithText(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: text},
			},
		},
	}
}

func TestRunSubAgent(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	t.Run("happy path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			assert.Equal(t, "do something", call.Prompt)
			assert.Equal(t, int64(4096), call.MaxOutputTokens)
			return agentResultWithText("done"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "do something",
			SessionTitle:   "Test Session",
		})
		require.NoError(t, err)
		assert.Equal(t, "done", resp.Content)
		assert.False(t, resp.IsError)
	})

	t.Run("cost update failure preserves output", func(t *testing.T) {
		// A failure to charge the parent session must not discard the
		// sub-agent output that was already produced. Using a parent
		// SessionID that was never created makes IncrementCost fail.
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("output before cost failure"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      "missing-parent-session",
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.Equal(t, "output before cost failure", resp.Content)
	})

	t.Run("nil result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("empty result returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return &fantasy.AgentResult{}, nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Sub-agent completed but produced no text output.", resp.Content)
	})

	t.Run("ModelCfg.MaxTokens overrides default", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := &mockSessionAgent{
			model: Model{
				CatwalkCfg: catwalk.Model{
					DefaultMaxTokens: 4096,
				},
				ModelCfg: config.SelectedModel{
					Provider:  providerID,
					MaxTokens: 8192,
				},
			},
			runFunc: func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				assert.Equal(t, int64(8192), call.MaxOutputTokens)
				return agentResultWithText("ok"), nil
			},
		}

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Content)
	})

	t.Run("session creation failure with canceled context", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, nil)

		// Use a canceled context to trigger CreateTaskSession failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = coord.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
	})

	t.Run("provider not configured", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Agent references a provider that doesn't exist in config.
		agent := newMockAgent("unknown-provider", 4096, nil)

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "model provider not configured")
	})

	t.Run("agent run error returns error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("provider request failed")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		// runSubAgent returns (errorResponse, nil) when agent.Run fails — not a Go error.
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.Equal(t, "Failed to generate response: provider request failed", resp.Content)
	})

	// Gap 1 (docs/plans/2026-07-26-orchestrator-worker-e2e.md, section
	// "Фаза 3"): a worker calling ask_question stops its turn via
	// AwaitingAnswerError (see agent.go's errors.As normalization of
	// tools.AskQuestionError), not a genuine failure. runSubAgent must
	// recognize this BEFORE the generic error branch and return a normal
	// SUCCESSFUL tool response shaped as a question, so the orchestrator
	// doesn't mistake a paused sub-agent for a crashed one and redo its
	// work. mockSessionAgent.Run returning *AwaitingAnswerError directly is
	// the lightest way to simulate what agent.Run would have normalized
	// tools.AskQuestionError into.
	t.Run("worker question produces a question-shaped SUCCESSFUL response, not a failure", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var capturedChildSessionID string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			capturedChildSessionID = call.SessionID
			return nil, &AwaitingAnswerError{
				Question:  "What timeout value should I use?",
				Options:   []string{"30s", "60s"},
				SessionID: call.SessionID,
			}
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "fix the config",
			SessionTitle:   "Test",
		})
		require.NoError(t, err, "runSubAgent must not return a Go error for a worker question")
		require.False(t, resp.IsError, "the tool response must be SUCCESSFUL, not an error, otherwise the orchestrator reads it as a crash and redoes the work")
		assert.NotContains(t, resp.Content, "Failed to generate response", "must not be framed as a failure")
		assert.Contains(t, resp.Content, "SUB-AGENT QUESTION")
		assert.Contains(t, resp.Content, capturedChildSessionID, "the child session id must appear so the orchestrator can resume it")
		assert.Contains(t, resp.Content, "What timeout value should I use?", "the question text must appear")
		assert.Contains(t, resp.Content, "Suggested options: 30s | 60s", "options must render when present")
		assert.Contains(t, resp.Content, "resume_session_id", "guidance must mention how to resume")
	})

	t.Run("worker question with no options omits the Suggested options line", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, &AwaitingAnswerError{
				Question:  "Should I proceed?",
				SessionID: call.SessionID,
			}
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "fix the config",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.False(t, resp.IsError)
		assert.NotContains(t, resp.Content, "Suggested options", "must not print an options line when there are none")
	})

	// Regression guard (required test (b)): a genuine sub-agent failure (any
	// error that is NOT an *AwaitingAnswerError) must still produce the old
	// generic error response.
	t.Run("genuine failure (not a question) still produces the generic error response", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return nil, errors.New("connection reset by peer")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError, "a genuine failure must still be reported as a tool error")
		assert.Equal(t, "Failed to generate response: connection reset by peer", resp.Content)
		assert.NotContains(t, resp.Content, "SUB-AGENT QUESTION")
	})

	// Required test (а): a sub-agent that fails with a genuine error (not
	// ask_question) must STILL charge the parent for whatever it spent before
	// failing. The old code returned the error response without charging,
	// permanently losing that cost; the transactional ledger now charges on
	// every outcome.
	t.Run("genuine failure still charges the parent for the cost accrued", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// The sub-agent spends something before it fails.
			if _, err := env.sessions.IncrementCost(ctx, call.SessionID, 0.04); err != nil {
				return nil, err
			}
			return nil, errors.New("connection reset by peer")
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError, "the failure must still be surfaced as a tool error")
		assert.Equal(t, "Failed to generate response: connection reset by peer", resp.Content)

		// Despite the error, the parent must have been charged the 0.04 the
		// child spent. This is the regression: previously this was 0.0.
		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.04, updated.Cost, 1e-9,
			"a failed sub-agent run must still charge the parent for the cost it accrued before failing")
	})

	// Gap 2 (Phase 3.2): resume_session_id must continue the SAME session -
	// the session id passed to Agent.Run must match the existing child
	// session, and the prior conversation persisted by the first call must
	// still be there afterward (proving no fresh session was minted).
	t.Run("resume_session_id continues the same session with prior context intact", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var firstCallSessionID string
		firstAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			firstCallSessionID = call.SessionID
			// Simulate the sub-agent doing some work and persisting a
			// message in its own session before it pauses on a question.
			_, msgErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
				Role:  message.Assistant,
				Parts: []message.ContentPart{message.TextContent{Text: "already did step 1"}},
			})
			require.NoError(t, msgErr)
			return nil, &AwaitingAnswerError{Question: "which port?", SessionID: call.SessionID}
		})

		firstResp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          firstAgent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "configure the server",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)
		require.False(t, firstResp.IsError)
		require.Contains(t, firstResp.Content, firstCallSessionID)

		var secondCallSessionID string
		secondAgent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			secondCallSessionID = call.SessionID
			assert.Equal(t, "8080", call.Prompt, "the answer must be forwarded as the prompt")
			return agentResultWithText("configured port 8080"), nil
		})

		resumeResp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           secondAgent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-2",
			ToolCallID:      "call-2",
			Prompt:          "8080",
			SessionTitle:    "Test",
			ResumeSessionID: firstCallSessionID,
		})
		require.NoError(t, err)
		assert.False(t, resumeResp.IsError)
		assert.Equal(t, firstCallSessionID, secondCallSessionID, "resume must reuse the SAME session id, not mint a new one")

		// The prior conversation from before the pause must still be there.
		msgs, err := env.messages.List(t.Context(), firstCallSessionID)
		require.NoError(t, err)
		found := false
		for _, m := range msgs {
			if m.Content().Text == "already did step 1" {
				found = true
			}
		}
		assert.True(t, found, "resuming must preserve the sub-agent's prior conversation, not discard it")
	})

	// Gap 2 security check (required test (d)): resuming a session that is
	// not a child of the CURRENT parent session must be rejected with a
	// tool error, and must not touch that unrelated session (no new message
	// appended to it).
	t.Run("resume_session_id that is not a child of the current session is rejected", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		otherParent, err := env.sessions.Create(t.Context(), "Unrelated parent")
		require.NoError(t, err)
		unrelatedChild, err := env.sessions.CreateTaskSession(t.Context(), "unrelated-child", otherParent.ID, "Unrelated child")
		require.NoError(t, err)

		var runInvoked bool
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			runInvoked = true
			return agentResultWithText("should not happen"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "answer",
			SessionTitle:    "Test",
			ResumeSessionID: unrelatedChild.ID,
		})
		require.NoError(t, err, "a bad resume id is a retryable tool error, not a Go error/crash")
		assert.True(t, resp.IsError)
		assert.False(t, runInvoked, "the sub-agent must never run against a session that doesn't belong to this caller")

		msgs, err := env.messages.List(t.Context(), unrelatedChild.ID)
		require.NoError(t, err)
		assert.Empty(t, msgs, "the unrelated session must not be touched")
	})

	t.Run("resume_session_id that does not exist is rejected", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var runInvoked bool
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			runInvoked = true
			return agentResultWithText("should not happen"), nil
		})

		resp, err := coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           agent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-1",
			ToolCallID:      "call-1",
			Prompt:          "answer",
			SessionTitle:    "Test",
			ResumeSessionID: "does-not-exist",
		})
		require.NoError(t, err)
		assert.True(t, resp.IsError)
		assert.False(t, runInvoked)
	})

	t.Run("cost accounting is not skipped on the resume path", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		// Cost BEFORE the pause must be non-zero: a zero pre-pause cost
		// makes the double-charge bug (parent gets A + (A+B) instead of
		// A+B) indistinguishable from correct behavior, since 2*0+B == B.
		var childSessionID string
		firstAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			childSessionID = call.SessionID
			if _, err := env.sessions.IncrementCost(ctx, call.SessionID, 0.03); err != nil {
				return nil, err
			}
			return nil, &AwaitingAnswerError{Question: "q?", SessionID: call.SessionID}
		})
		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          firstAgent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "start",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		afterPauseParent, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.03, afterPauseParent.Cost, 1e-9, "the pre-pause cost must reach the parent exactly once")

		resumeAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate cost accruing on the resumed turn, ON TOP of the
			// 0.03 the child already carries from before the pause.
			if _, err := env.sessions.IncrementCost(ctx, call.SessionID, 0.02); err != nil {
				return nil, err
			}
			return agentResultWithText("done"), nil
		})
		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:           resumeAgent,
			SessionID:       parentSession.ID,
			AgentMessageID:  "msg-2",
			ToolCallID:      "call-2",
			Prompt:          "answer",
			SessionTitle:    "Test",
			ResumeSessionID: childSessionID,
		})
		require.NoError(t, err)

		updatedParent, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updatedParent.Cost, 1e-9,
			"parent must be charged the child's total (0.03+0.02), not double-charged the pre-pause portion (0.03+0.05=0.08)")
	})

	t.Run("session setup callback is invoked", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		var setupCalledWith string
		agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
			SessionSetup: func(sessionID string) {
				setupCalledWith = sessionID
			},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, setupCalledWith, "SessionSetup should have been called")
	})

	t.Run("cost propagation to parent session", func(t *testing.T) {
		env := testEnv(t)
		coord := newTestCoordinator(t, env, providerID, providerCfg)

		parentSession, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		agent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			// Simulate the agent incurring cost on the child session via the
			// race-safe additive API (Save no longer writes the cost column).
			if _, err := env.sessions.IncrementCost(ctx, call.SessionID, 0.05); err != nil {
				return nil, err
			}
			return agentResultWithText("ok"), nil
		})

		_, err = coord.runSubAgent(t.Context(), subAgentParams{
			Agent:          agent,
			SessionID:      parentSession.ID,
			AgentMessageID: "msg-1",
			ToolCallID:     "call-1",
			Prompt:         "test",
			SessionTitle:   "Test",
		})
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parentSession.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, updated.Cost, 1e-9)
	})
}
