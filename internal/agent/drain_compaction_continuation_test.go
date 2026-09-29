// B4 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a Drain
// turn that triggers auto-summarize mid-stream (pending tool calls +
// crossed context-window threshold) must still deliver its post-compaction
// continuation to the provider. Before the fix, agent_turn.go's
// shouldSummarize branch copied `call` (keeping IsDrain=true,
// drainTurnCommitted=false) into the continuation; the NEXT runTurn
// iteration re-gated on decideDrainTurn, found the debt the first step's
// own persistStepFinish had already marked reacted=1, took the no-turn
// branch, and ended the whole run WITHOUT ever sending the continuation --
// the compaction's own pending tool call left permanently unanswered.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrain_CompactionContinuation_ReachesProviderAfterAutoSummarize is the
// end-to-end regression test for B4.
//
// REVERT CHECK: removed `continuationCall.drainTurnCommitted =
// continuationCall.IsDrain` from agent_turn.go's shouldSummarize branch --
// this test's `continuationCalls.Load()` assertion FAILED (stayed 0; the
// run instead ended via the no-turn branch with a nil result). Restored the
// line; re-ran, passed.
func TestDrain_CompactionContinuation_ReachesProviderAfterAutoSummarize(t *testing.T) {
	var mainCalls, summarizeCalls, continuationCalls atomic.Int64

	// Main Drain turn: a pending tool call AND usage crossing the
	// auto-summarize threshold in the SAME finishing chunk (ContextWindow
	// 1000, smallContextWindowRatio 0.2 -> threshold 200; total 800 ->
	// remaining 200 <= 200 fires shouldSummarize). This step's real content
	// (a tool call) makes persistStepFinish mark the Drain's pulled notice
	// reacted=1 -- exactly the precondition that later makes the naive
	// continuation re-gate find no visible debt.
	mainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mainCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"probe_tool","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":600,"completion_tokens":200,"total_tokens":800}}`,
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
	t.Cleanup(mainSrv.Close)

	summarizeSrv := summarizeSSEServer(5, 2, &summarizeCalls)
	t.Cleanup(summarizeSrv.Close)
	continuationSrv := summarizeSSEServer(5, 2, &continuationCalls)
	t.Cleanup(continuationSrv.Close)

	dispatchSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case mainCalls.Load() == 0:
			mainSrv.Config.Handler.ServeHTTP(w, r)
		case summarizeCalls.Load() == 0:
			summarizeSrv.Config.Handler.ServeHTTP(w, r)
		default:
			continuationSrv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(dispatchSrv.Close)
	lm := languageModelFromServer(t, dispatchSrv)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 1000}}

	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	ledger := newWorkLedger(nil)
	ledger.store = store

	sa := NewSessionAgent(SessionAgentOptions{
		DataDirectory: env.workingDir,
		SmartModel:    model, FastModel: model, SystemPrompt: "you are a probe",
		Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, AsyncJobs: ledger,
		// DisableAutoSummarize intentionally left false: the whole point is
		// to exercise the real shouldSummarize gate.
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-compaction-continuation")
	require.NoError(t, err)

	// Seed real VISIBLE debt: claim+finish a job, pull its notice into
	// history -- the shape decideDrainTurn's first (main) evaluation needs.
	_, existing, err := ledger.Start(sess.ID, "call-async-1", "call-async-1", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "call-async-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-async-1", State: "completed", ResultSummary: "async output", Wake: true,
	})
	require.NoError(t, err)
	pulled, err := store.PullJobNotices(ctx, env.messages, sess.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)

	drainCall := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	result, err := sa.Run(ctx, drainCall)
	require.NoError(t, err)

	require.Equal(t, int64(1), mainCalls.Load(), "main Drain turn must execute exactly once")
	require.Equal(t, int64(1), summarizeCalls.Load(), "compaction must execute exactly once")
	require.Equal(t, int64(1), continuationCalls.Load(),
		"the post-compaction continuation must reach the provider exactly once -- it must not be silently dropped via the no-turn branch")
	require.NotNil(t, result, "a run that actually continued and finished must return a real result, not a bare (nil, nil) no-turn end")

	finalSess, err := env.sessions.Get(ctx, sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, finalSess.SummaryMessageID, "expected inline compaction to have created a summary")

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	var foundContinuationUserMsg bool
	for _, m := range msgs {
		if m.Role != message.User {
			continue
		}
		if strings.Contains(m.FullText(), "previous session was interrupted") {
			foundContinuationUserMsg = true
		}
	}
	require.True(t, foundContinuationUserMsg,
		"the continuation's synthesized prompt must be persisted as a real follow-on turn, proving it executed rather than being silently dropped")
}
