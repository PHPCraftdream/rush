// Task #1267 regression: a compaction whose Summarize hits an OS session lock
// held by another process must be re-queued (not silently dropped) and must
// run once the lock is free.
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
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP1267_LockBusyCompactionRequeuedAndRuns exercises the lock-busy branch,
// then releases the lock so the next owner can run the queued compaction.
//
// REVERT CHECK: removing the requeue in runSummarize's SessionLockBusyError
// branch makes the queued-state assertion fail before the next owner runs.
func TestP1267_LockBusyCompactionRequeuedAndRuns(t *testing.T) {
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
	var otherProcLock *session.SessionLock
	t.Cleanup(func() {
		if otherProcLock != nil {
			_ = otherProcLock.Release()
		}
		sessionAgent.CancelAll()
		sessionAgent.runWg.Wait()
	})

	sess, err := env.sessions.Create(ctx, "p1267-lock-busy-requeue")
	require.NoError(t, err)

	for i := range 8 {
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

	// Seed turn: establishes the session in a normal, completed state.
	_, err = sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "seed turn"})
	require.NoError(t, err)
	require.False(t, sessionAgent.IsSessionBusy(sess.ID), "session must be idle after the seed turn")

	// Hold the OS session lock, simulating ANOTHER process owning the
	// session. The byte-range/flock file lock blocks the in-process agent
	// exactly as it would a foreign process.
	otherProcLock, err = session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err)

	// Snapshot before taking the lock route; the queue is empty at this point.
	snapshot := sessionAgent.testBuildSummarizeSnapshot()
	require.False(t, sessionAgent.SummarizeQueued(sess.ID))

	require.ErrorIs(t, sa.Summarize(ctx, sess.ID, snapshot), ErrSummarizeQueued)
	// Busy branch returned without a provider request, and queue ownership stays parked.
	require.Equal(t, int64(1), totalCalls.Load())
	require.True(t, sessionAgent.SummarizeQueued(sess.ID))
	require.NoError(t, otherProcLock.Release())

	// Simulate the queued ownership handoff after the busy branch re-queued it.
	_, err = sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "next owner turn"})
	require.NoError(t, err)

	// Require a successfully committed summary, not merely its pre-stream row.
	require.Eventually(t, func() bool {
		updated, getErr := env.sessions.Get(ctx, sess.ID)
		if getErr != nil || updated.SummaryMessageID == "" {
			return false
		}
		msgs, listErr := env.messages.List(ctx, sess.ID)
		if listErr != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.ID == updated.SummaryMessageID {
				if finish := msg.FinishPart(); finish != nil && finish.Reason == message.FinishReasonEndTurn {
					return true
				}
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond,
		"summary must be committed and finished")

	// The agent run wait also joins the detached Summarize goroutine.
	sessionAgent.runWg.Wait()

	require.False(t, sessionAgent.SummarizeQueued(sess.ID),
		"summarize queue must be empty once the compaction has run")

	// Exactly three provider calls: seed turn, next-owner turn, compaction.
	require.Eventually(t, func() bool {
		return totalCalls.Load() >= 3
	}, 5*time.Second, 50*time.Millisecond,
		"expected exactly seed turn + next-owner turn + one compaction")
	require.Equal(t, int64(3), totalCalls.Load(),
		"expected exactly seed turn + next-owner turn + one compaction")
}
