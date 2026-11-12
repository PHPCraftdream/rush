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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestDrain_CompactionContinuation_NoEmptyPrompt is the end-to-end regression
// test for B4 (the continuation reaches the provider) and R2B-15 (a Drain's
// continuation persists no empty-request prompt).
//
// REVERT CHECK B4: removed `continuationCall.drainTurnCommitted =
// continuationCall.IsDrain` from agent_turn.go's shouldSummarize branch --
// the `continuationCalls.Load()` assertion FAILED (stayed 0).
// REVERT CHECK R2B-15: giving a Drain's continuation the request-template
// prompt again persists a "the initial user request was: “" user row and
// the NotContains assertion FAILED.
func TestDrain_CompactionContinuation_NoEmptyPrompt(t *testing.T) {
	var mainCalls, summarizeCalls, continuationCalls atomic.Int64
	var summaryRequests, ordinaryFollowUpRequests atomic.Int64

	// Main Drain turn: a pending tool call AND usage crossing the
	// auto-summarize threshold in the SAME finishing chunk. The model's
	// 200-token output reserve matches the fake response's completion usage,
	// leaving an 800-token input budget and a 160-token threshold; total usage
	// 800 leaves zero, so shouldSummarize fires. This step's real content
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot inspect test request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		isSummaryRequest := bytes.Contains(body, []byte("You are summarizing a conversation to preserve context"))
		switch {
		case mainCalls.Load() == 0:
			mainSrv.Config.Handler.ServeHTTP(w, r)
		case summarizeCalls.Load() == 0:
			if isSummaryRequest {
				summaryRequests.Add(1)
			} else {
				ordinaryFollowUpRequests.Add(1)
			}
			summarizeSrv.Config.Handler.ServeHTTP(w, r)
		default:
			if isSummaryRequest {
				summaryRequests.Add(1)
			} else {
				ordinaryFollowUpRequests.Add(1)
			}
			continuationSrv.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(dispatchSrv.Close)
	lm := languageModelFromServer(t, dispatchSrv)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 1000, DefaultMaxTokens: 200}}

	env := testEnv(t)
	coord := a2AttachBatchFacts(t, env)

	sa := NewSessionAgent(SessionAgentOptions{
		DataDirectory: env.workingDir,
		SmartModel:    model, FastModel: model, SystemPrompt: "you are a probe",
		Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, Coord: coord,
		// DisableAutoSummarize intentionally left false: the whole point is
		// to exercise the real shouldSummarize gate.
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-compaction-continuation")
	require.NoError(t, err)

	// Seed real VISIBLE debt: claim+finish a job, pull its notice into
	// history -- the shape decideDrainTurn's first (main) evaluation needs.
	seedRoot, seedChild := seedFixtureJobDebt(t, env.conn, sess.ID, "call-async-1", true)

	drainCall := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	result, err := sa.Run(ctx, drainCall)
	finalSess, sessionErr := env.sessions.Get(ctx, sess.ID)
	msgs, messagesErr := env.messages.List(ctx, sess.ID)
	var summaryRows, emptyUserRows, requestTemplateRows int
	for _, m := range msgs {
		if m.IsSummaryMessage {
			summaryRows++
		}
		if m.Role == message.User {
			if strings.TrimSpace(m.FullText()) == "" {
				emptyUserRows++
			}
			if strings.Contains(m.FullText(), "the initial user request was") {
				requestTemplateRows++
			}
		}
	}
	var debtDelivery string
	var debtWake, debtReacted, debtWakeAttempts, debtReactedFailed int64
	debtErr := env.conn.QueryRowContext(ctx, `SELECT delivery, wake, reacted, wake_attempts, reacted_failed FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND node_id = ?`, sess.ID, seedRoot, seedChild).Scan(&debtDelivery, &debtWake, &debtReacted, &debtWakeAttempts, &debtReactedFailed)
	busyAfterRun := sa.IsSessionBusy(sess.ID)
	if err != nil || mainCalls.Load() != 1 || summarizeCalls.Load() != 1 || summaryRequests.Load() != 1 || ordinaryFollowUpRequests.Load() != 1 || continuationCalls.Load() != 1 || result == nil || sessionErr != nil || finalSess.SummaryMessageID == "" || messagesErr != nil || emptyUserRows != 0 || requestTemplateRows != 0 {
		stage := "post-run assertions"
		switch {
		case err != nil:
			stage = "run returned error"
		case mainCalls.Load() != 1:
			stage = "main provider request"
		case summarizeCalls.Load() != 1 || summaryRequests.Load() != 1:
			stage = "summary request/compaction"
		case ordinaryFollowUpRequests.Load() != 1 || continuationCalls.Load() != 1 || result == nil:
			stage = "post-compaction continuation"
		case sessionErr != nil || messagesErr != nil:
			stage = "session history read"
		case finalSess.SummaryMessageID == "":
			stage = "summary publication"
		case emptyUserRows != 0 || requestTemplateRows != 0:
			stage = "Drain user-history invariant"
		}
		t.Logf("drain compaction continuation diagnostic: stage=%q run_result_present=%t main_requests=%d summary_marker_requests=%d summary_handler_requests=%d ordinary_follow_up_requests=%d continuation_handler_requests=%d session_read_error=%t summary_pointer_set=%t summary_rows=%d messages_read_error=%t empty_user_rows=%d request_template_user_rows=%d busy_after_run=%t debt_read_error=%t debt_delivery=%q debt_wake=%d debt_reacted=%d debt_wake_attempts=%d debt_reacted_failed=%d",
			stage, result != nil, mainCalls.Load(), summaryRequests.Load(), summarizeCalls.Load(), ordinaryFollowUpRequests.Load(), continuationCalls.Load(), sessionErr != nil, finalSess.SummaryMessageID != "", summaryRows, messagesErr != nil, emptyUserRows, requestTemplateRows, busyAfterRun, debtErr != nil, debtDelivery, debtWake, debtReacted, debtWakeAttempts, debtReactedFailed)
	}

	require.NoError(t, err)

	require.Equal(t, int64(1), mainCalls.Load(), "main Drain turn must execute exactly once")
	require.Equal(t, int64(1), summarizeCalls.Load(), "compaction must execute exactly once")
	require.Equal(t, int64(1), summaryRequests.Load(), "the summary request marker must be present exactly once")
	require.Equal(t, int64(1), ordinaryFollowUpRequests.Load(), "exactly one ordinary provider request must follow compaction")
	require.Equal(t, int64(1), continuationCalls.Load(),
		"the post-compaction continuation must reach the provider exactly once -- it must not be silently dropped via the no-turn branch")
	require.NotNil(t, result, "a run that actually continued and finished must return a real result, not a bare (nil, nil) no-turn end")

	require.NoError(t, sessionErr)
	require.NotEmpty(t, finalSess.SummaryMessageID, "expected inline compaction to have created a summary")

	require.NoError(t, messagesErr)
	for _, m := range msgs {
		if m.Role != message.User {
			continue
		}
		require.NotEmpty(t, strings.TrimSpace(m.FullText()), "a Drain's continuation must never persist an empty user row")
		require.NotContains(t, m.FullText(), "the initial user request was",
			"a Drain has no user request: its continuation must not persist the request template with an empty prompt")
	}
}
