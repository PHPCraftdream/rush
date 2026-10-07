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

// TestP1267_LockBusyCompactionRequeuedAndRuns exercises the REAL sessionAgent
// path agent_compaction.go runSummarize lock-busy branch: with the OS session
// lock held by "another process" (the test itself), Summarize must return an
// error wrapping ErrSummarizeQueued, keep the snapshot in summarizeQueue
// (SummarizeQueued true), and the next owner of the session (a Run whose
// cleanup defer drains the queue) must execute the compaction once the lock
// is released.
//
// REVERT CHECK: removing the a.requeueSummarizeSnapshot(sessionID, snapshot)
// call in runSummarize's SessionLockBusyError branch makes this test fail at
// the SummarizeQueued assertion (the snapshot is dropped, SummarizeQueued
// stays false, and no summary message is ever written).
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
	otherProcLock, err := session.TryAcquireSessionLock(env.workingDir, sess.ID)
	require.NoError(t, err)

	// Snapshot before taking the lock route; the queue is empty at this point,
	// exactly as it is for the detached Summarize spawned by Run's cleanup
	// defer — the same empty-queue precondition the busy branch must survive.
	snapshot := sessionAgent.testBuildSummarizeSnapshot()
	require.False(t, sessionAgent.SummarizeQueued(sess.ID))

	// Manual Summarize on the idle mailbox reaches runSummarize and loses
	// the lock race to the held lock.
	summarizeErr := sa.Summarize(ctx, sess.ID, snapshot)

	// Release the lock: from here the session is free again, and the
	// re-queued compaction must be runnable by the next owner.
	require.NoError(t, otherProcLock.Release())

	require.Error(t, summarizeErr, "Summarize must report the lock failure")
	require.ErrorIs(t, summarizeErr, ErrSummarizeQueued,
		"the failed compaction must be reported as re-queued, not lost")

	// The defect: pre-fix, the busy path dropped the snapshot and
	// SummarizeQueued stayed false forever.
	require.True(t, sessionAgent.SummarizeQueued(sess.ID),
		"compaction must stay queued after a lock-busy failure")

	// No busy retry loop: the queue must not self-drain while the compaction
	// is merely parked (nothing spawned on the lock-busy path).
	time.Sleep(200 * time.Millisecond)
	require.True(t, sessionAgent.SummarizeQueued(sess.ID),
		"re-queued compaction must wait for the next owner, not spin")

	// Next owner of the session: a Run whose cleanup defer drains the
	// summarize queue and executes the compaction.
	_, err = sa.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "next owner turn"})
	require.NoError(t, err)

	// The re-queued compaction runs detached; wait for the summary message
	// itself, not for the queue to drain (the queue empties when the
	// compaction is spawned, not when it completes).
	require.Eventually(t, func() bool {
		msgs, err := env.messages.List(ctx, sess.ID)
		if err != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.IsSummaryMessage {
				return true
			}
		}
		return false
	}, 15*time.Second, 50*time.Millisecond,
		"summary message must exist after the re-queued compaction ran")

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
