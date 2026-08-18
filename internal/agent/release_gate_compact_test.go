// Release gate test for manual /compact coalescing: a second compact queued
// while the first runs is drained automatically when the first succeeds.
// Holds TestReleaseGate_4_SecondCompactCoalesced.

package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestReleaseGate_4_SecondCompactCoalesced proves that a second manual /compact
// queued during the first is drained/discarded automatically upon successful completion
// of the first, without test intervention.
//
// CRITERION: Second /compact during first → coalesced/drained WITHOUT test involvement;
//
//	SummarizeQueued() reports false after.
//
// NO EXTERNAL POKE: Test queues second /compact, waits for first to complete autonomously.
// The coalesce/drain happens automatically in the success path. We do NOT manually drain.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go runSummarize success path, remove the summarizeQueue drain block
//  2. Run: go test -run TestReleaseGate_4_SecondCompactCoalesced -v
//  3. FAIL: SummarizeQueued returns true after first /compact, second /compact stuck
//  4. Restore the drain block and PASS
func TestReleaseGate_4_SecondCompactCoalesced(t *testing.T) {
	t.Parallel()

	var totalCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"response"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "test-model")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg: config.SelectedModel{
			Provider: "openaicompat",
			Model:    "test-model",
		},
	}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		SystemPrompt:         "you are an assistant",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "release-gate-4")
	require.NoError(t, err)

	// Create enough messages to trigger summarization (4 user + 4 assistant).
	for i := 0; i < 8; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
			Role:  role,
			Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message %d", i)}},
		})
		require.NoError(t, err)
	}

	// Start a normal Run to establish ownership.
	_, err = sa.Run(ctx, SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "test message to establish ownership",
	})
	require.NoError(t, err)

	// PHASE 1: Start first /compact - it should acquire ownership immediately.
	summarizeErr := make(chan error, 1)
	summarizeStarted := make(chan struct{}, 1)
	go func() {
		summarizeStarted <- struct{}{}
		summarizeErr <- sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	}()

	// Wait for the first /compact to actually start and acquire ownership.
	<-summarizeStarted
	require.Eventually(t, func() bool {
		return sessionAgent.IsSessionBusy(sess.ID)
	}, 2*time.Second, 50*time.Millisecond, "session must become busy during first /compact")

	// Verify session is busy.
	require.True(t, sessionAgent.IsSessionBusy(sess.ID), "session must be busy during first /compact")

	// PHASE 2: Queue a second /compact while the first is still running.
	// This should return ErrSummarizeQueued.
	secondCompactErr := sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	require.ErrorIs(t, secondCompactErr, ErrSummarizeQueued, "second /compact must be queued")

	// Verify the second /compact is in the queue.
	require.True(t, sessionAgent.SummarizeQueued(sess.ID), "summarizeQueue must hold the pending second /compact")

	// Wait for first /compact to complete autonomously - NO manual intervention.
	require.Eventually(t, func() bool {
		select {
		case err := <-summarizeErr:
			require.NoError(t, err, "first /compact must complete successfully")
			return true
		default:
			return false
		}
	}, 10*time.Second, 100*time.Millisecond, "first /compact must complete within timeout")

	// PHASE 3: Verify the queued entry was coalesced/drained AUTOMATICALLY.
	require.Eventually(t, func() bool {
		return !sessionAgent.SummarizeQueued(sess.ID)
	}, 5*time.Second, 50*time.Millisecond,
		"summarizeQueue must be empty after first /compact completes (coalesced)")

	// Verify that we don't have a runaway compaction.
	time.Sleep(200 * time.Millisecond)

	// Verify session is idle again.
	require.False(t, sessionAgent.IsSessionBusy(sess.ID), "session must be idle after first /compact completes")

	// Verify SummarizeQueued() reports false.
	queuedState := sessionAgent.SummarizeQueued(sess.ID)
	require.False(t, queuedState, "SummarizeQueued() must report false after coalesce")

	// Verify total provider calls: exactly 2 (Run + first /compact).
	require.Equal(t, int64(2), totalCalls.Load(), "should only see 2 LLM calls (Run + first /compact), not a third")

	// Verify a third /compact works normally (queue is genuinely empty).
	thirdCompactErr := sa.Summarize(ctx, sess.ID, sessionAgent.testBuildSummarizeSnapshot())
	require.NoError(t, thirdCompactErr, "third /compact must succeed (queue is genuinely empty)")
}
