// End-to-end coverage for phase-4 step 3's Drain call and driver pull
// (docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.3/3.4), plus
// unit coverage for the queue-merge/durable-enqueue/QueuedPromptsList
// exclusions that make a Drain call "minimal" per the step's own scope note.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// --- mergeQueuedCall / QueuedPromptsList / durable-enqueue unit coverage ---

// TestMergeQueuedCall_AtMostOneDrainInQueue pins doc sec.3.4: at most one
// Drain call in a session's queue on any insertion path.
//
// Revert-check performed: changed mergeQueuedCall's IsDrain branch to
// unconditionally `return append(queued, call)` (never checking
// len(queued) > 0) -- this test FAILED (queue held two Drain entries after
// two drainCall submissions to an already-queued session). Restored the
// guard; re-ran, passed.
func TestMergeQueuedCall_AtMostOneDrainInQueue(t *testing.T) {
	drain1 := SessionAgentCall{SessionID: "s1", IsDrain: true}
	drain2 := SessionAgentCall{SessionID: "s1", IsDrain: true}

	queued := mergeQueuedCall(nil, drain1)
	require.Len(t, queued, 1)
	queued = mergeQueuedCall(queued, drain2)
	require.Len(t, queued, 1, "a second Drain must not grow the queue past one")
}

// TestMergeQueuedCall_UserCallDropsQueuedDrain pins doc sec.3.4: "when it
// merges with a regular user call, the user call runs and the Drain
// disappears".
func TestMergeQueuedCall_UserCallDropsQueuedDrain(t *testing.T) {
	drain := SessionAgentCall{SessionID: "s1", IsDrain: true}
	user := SessionAgentCall{SessionID: "s1", Prompt: "hello"}

	queued := mergeQueuedCall(nil, drain)
	require.Len(t, queued, 1)
	queued = mergeQueuedCall(queued, user)
	require.Len(t, queued, 1)
	require.False(t, queued[0].IsDrain)
	require.Equal(t, "hello", queued[0].Prompt)
}

// TestMergeQueuedCall_DrainDroppedWhenQueueNonEmpty pins doc sec.3.4: an
// incoming Drain never grows a non-empty queue -- whatever runs next already
// pulls at its own turn start, making a second Drain redundant.
func TestMergeQueuedCall_DrainDroppedWhenQueueNonEmpty(t *testing.T) {
	user := SessionAgentCall{SessionID: "s1", Prompt: "hello"}
	drain := SessionAgentCall{SessionID: "s1", IsDrain: true}

	queued := mergeQueuedCall(nil, user)
	queued = mergeQueuedCall(queued, drain)
	require.Len(t, queued, 1)
	require.False(t, queued[0].IsDrain, "a Drain must never be queued behind an existing call")
}

// TestQueuedPromptsList_ExcludesDrain pins the requirement that a queued
// Drain call never surfaces in the operator-facing queue list.
//
// Revert-check performed: removed the `if call.IsDrain { continue }` guard
// in QueuedPromptsList -- this test FAILED (list contained an empty string
// for the Drain entry). Restored the guard; re-ran, passed.
func TestQueuedPromptsList_ExcludesDrain(t *testing.T) {
	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{CatwalkCfg: catwalk.Model{ContextWindow: 1000}}, FastModel: Model{},
		Sessions: env.sessions, Messages: env.messages, DataDirectory: env.workingDir,
	})
	mb := sa.(*sessionAgent).getMailbox("s1")
	mb.mu.Lock()
	mb.submitted = append(mb.submitted, SessionAgentCall{SessionID: "s1", IsDrain: true}, SessionAgentCall{SessionID: "s1", Prompt: "visible"})
	mb.mu.Unlock()

	list := sa.QueuedPromptsList("s1")
	require.Equal(t, []string{"visible"}, list)
}

// TestDrainCall_NeverDurablyEnqueued pins doc sec.3.4: "никогда не ставится
// в долговременную очередь". restartOrphanedWithRetry is the production path
// that durably enqueues an orphaned call on handoff failure; a Drain call
// fed through it must produce NO row in the durable run queue.
//
// Revert-check performed: removed the `if call.IsDrain { return }` guard in
// restartOrphanedWithRetry's per-call goroutine -- this test FAILED (one row
// appeared in ListPendingRunQueueEntries). Restored the guard; re-ran,
// passed.
func TestDrainCall_NeverDurablyEnqueued(t *testing.T) {
	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{CatwalkCfg: catwalk.Model{ContextWindow: 1000}}, FastModel: Model{},
		Sessions: env.sessions, Messages: env.messages, DataDirectory: env.workingDir,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-no-durable-queue")
	require.NoError(t, err)

	drain := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	require.NoError(t, sessionAgent.restartOrphanedWithRetry([]SessionAgentCall{drain}))

	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "a Drain call must never be durably enqueued")
}

// --- end-to-end Drain / notice-pull coverage ---

// sseChunk writes one SSE data chunk.
func sseChunk(w http.ResponseWriter, fl http.Flusher, s string) {
	fmt.Fprintf(w, "data: %s\n\n", s)
	if fl != nil {
		fl.Flush()
	}
}

// textFinishResponse serves a single-step plain-text assistant reply.
func textFinishResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, text))
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func newProbeModel(t *testing.T, srv *httptest.Server) Model {
	t.Helper()
	return newProbeModelClient(t, srv, nil)
}

// httpDoer is the HTTP client seam of the provider SDK.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// newProbeModelClient is newProbeModel over a caller-supplied HTTP client
// (nil: the SDK default), e.g. one with a Client.Timeout.
func newProbeModelClient(t *testing.T, srv *httptest.Server, client httpDoer) Model {
	t.Helper()
	opts := []openaicompat.Option{openaicompat.WithBaseURL(srv.URL), openaicompat.WithAPIKey("probe")}
	if client != nil {
		opts = append(opts, openaicompat.WithHTTPClient(client))
	}
	provider, err := openaicompat.New(opts...)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	return Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
}

// TestDrain_WakeNoticeCallsProviderWithNoticeAndNoTextPrompt pins the
// Drain-with-something-to-react-to branch (doc sec.3.4): the turn's own
// prompt is empty, the reaction is purely to the notice the turn-start pull
// (agent_notice_pull.go) already spliced into history, and no second,
// separate user row is ever persisted.
//
// Revert-check performed: temporarily forced skipUserMessage to always
// false in agent_turn.go's createUserMessage guard -- this test FAILED (two
// user-role messages existed: the notice AND an empty-text row from
// createUserMessage). Restored the guard; re-ran, passed.
func TestDrain_WakeNoticeCallsProviderWithNoticeAndNoTextPrompt(t *testing.T) {
	t.Parallel()
	var capturedBody []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mu.Lock()
		capturedBody = body
		mu.Unlock()
		textFinishResponse(w, "acknowledged")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	ledger := newWorkLedger(nil)
	ledger.store = asyncStore

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-wake-test")
	require.NoError(t, err)

	_, err = asyncStore.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, asyncStore.MarkAnnounced(ctx, sess.ID, "call-1"))
	_, err = asyncStore.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "job output", Wake: true,
	})
	require.NoError(t, err)

	drainCall := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	result, err := sessionAgent.Run(ctx, drainCall)
	require.NoError(t, err)
	require.NotNil(t, result, "a Drain that pulled a wake=1 notice must reach the provider")

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	var userMsgs, noticeMsgs int
	for _, m := range msgs {
		if m.Role == message.User {
			userMsgs++
		}
		if m.BackgroundJobNotice {
			noticeMsgs++
		}
	}
	require.Equal(t, 1, userMsgs, "exactly one user-role message (the pulled notice) -- no separate empty prompt row")
	require.Equal(t, 1, noticeMsgs)

	mu.Lock()
	body := string(capturedBody)
	mu.Unlock()
	require.Contains(t, body, "job output", "the provider must see the notice text")

	row, err := asyncStore.Get(ctx, sess.ID, "call-1")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
}

// TestDrain_NothingPendingSkipsProviderAndFinishesTurn pins the "no reaction
// debt" branch (doc sec.3.4): no provider call, no empty assistant message,
// normal turn end via drainOrReleaseMerged.
//
// Revert-check performed: removed the `if call.IsDrain && !anyWake` early
// return in agent_turn.go's runTurn -- this test FAILED (the provider's
// request handler was invoked with an empty prompt, and an assistant
// message was created). Restored the guard; re-ran, passed.
func TestDrain_NothingPendingSkipsProviderAndFinishesTurn(t *testing.T) {
	t.Parallel()
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		textFinishResponse(w, "should never be sent")
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	env := testEnv(t)
	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	ledger := newWorkLedger(nil)
	ledger.store = asyncStore

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true, AsyncJobs: ledger,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "drain-empty-test")
	require.NoError(t, err)

	drainCall := newDrainCall(SessionAgentCall{SessionID: sess.ID})
	result, err := sessionAgent.Run(ctx, drainCall)
	require.NoError(t, err)
	require.Nil(t, result, "a Drain with nothing to pull must never reach the provider")

	mu.Lock()
	n := calls
	mu.Unlock()
	require.Zero(t, n, "the provider must never be called")

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, msgs, "no assistant or user message may be created")
}

// TestDrain_NoticeBetweenTwoToolCalls_OnlyAtStepBoundaryAndCarriesForward
// pins DUR-3a (a notice never lands between a tool call and its own result)
// and the carry-forward requirement (doc sec.3.4's second bullet): a notice
// spliced in at one step's boundary must still be visible to the PROVIDER's
// request body at every later step of the same turn, not just the one right
// after it landed.
//
// Sequence: step 1 calls tool_a, whose Run() commits the async job's
// completion (making the notice pending) WHILE the tool is executing --
// i.e. strictly between the model's tool_call and fantasy's own delivery of
// that call's result back to the model. Step 2's PrepareStep pulls the
// notice (turn start already ran with nothing pending) and splices it in,
// then the model calls tool_b. Step 3 is the final text response.
//
// Revert-check performed: removed the carry-forward re-append
// (ts.carriedSplices) from agent_turn_step.go's prepareStep, keeping only
// the step-2 splice -- this test FAILED (step 3's captured request body no
// longer contained the notice text, proving it had been silently dropped
// from the model's context for that step). Restored the carry-forward
// logic; re-ran, passed.
func TestDrain_NoticeBetweenTwoToolCalls_OnlyAtStepBoundaryAndCarriesForward(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	asyncStore := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	ledger := newWorkLedger(nil)
	ledger.store = asyncStore

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "dur3a-carryforward-test")
	require.NoError(t, err)
	_, err = asyncStore.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, asyncStore.MarkAnnounced(ctx, sess.ID, "call-1"))

	var mu sync.Mutex
	var bodies []string
	var step int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		mu.Lock()
		bodies = append(bodies, string(buf))
		s := step
		step++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		switch s {
		case 0:
			sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":"{}"}}]},"finish_reason":null}]}`)
			sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		case 1:
			sseChunk(w, fl, `{"id":"c2","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"tool_b","arguments":"{}"}}]},"finish_reason":null}]}`)
			sseChunk(w, fl, `{"id":"c2","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		default:
			sseChunk(w, fl, `{"id":"c3","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"final"},"finish_reason":null}]}`)
			sseChunk(w, fl, `{"id":"c3","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	model := newProbeModel(t, srv)

	toolA := &completionTriggerTool{name: "tool_a", store: asyncStore, owner: sess.ID, toolCallID: "call-1"}
	toolB := &completionTriggerTool{name: "tool_b"}

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "you are a probe",
		DataDirectory: env.workingDir, Sessions: env.sessions, Messages: env.messages,
		Tools: []fantasy.AgentTool{toolA, toolB}, DisableAutoSummarize: true, AsyncJobs: ledger,
		IsYolo: true,
	})
	sessionAgent := sa.(*sessionAgent)

	// AutoResumed suppresses title generation (agent_turn.go's needsTitle),
	// which would otherwise fire a concurrent request against the SAME test
	// server and corrupt this test's sequential step-index branching below.
	_, err = sessionAgent.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "go", AutoResumed: true})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	var order []string
	noticeIdx, toolAIdx, toolBResultIdx := -1, -1, -1
	for i, m := range msgs {
		if m.BackgroundJobNotice {
			noticeIdx = i
		}
		text := m.FullText()
		if strings.Contains(text, "call_a") || (m.Role == message.Assistant && len(m.ToolCalls()) > 0 && m.ToolCalls()[0].Name == "tool_a") {
			toolAIdx = i
		}
		if m.Role == message.Tool {
			for _, tr := range m.ToolResults() {
				if tr.ToolCallID == "call_a" {
					toolAIdx = i
				}
				if tr.ToolCallID == "call_b" {
					toolBResultIdx = i
				}
			}
		}
		order = append(order, fmt.Sprintf("%d:%s", i, m.Role))
	}
	require.NotEqual(t, -1, noticeIdx, "notice message must exist: %v", order)
	require.NotEqual(t, -1, toolAIdx, "tool_a's own message must exist: %v", order)
	require.Greater(t, noticeIdx, toolAIdx, "the notice must land AFTER tool_a's own tool-call/result pair, never inside it")
	if toolBResultIdx != -1 {
		require.Less(t, noticeIdx, toolBResultIdx, "the notice must land BEFORE tool_b's result -- i.e. at the step-2 boundary")
	}

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(bodies), 3, "the turn must have reached a third step")
	require.NotContains(t, bodies[0], "job output", "step 1's request precedes the job's completion entirely")
	require.Contains(t, bodies[1], "job output", "step 2's request must see the notice spliced in at its own boundary")
	require.Contains(t, bodies[2], "job output", "step 3's request must STILL see the notice -- carry-forward (doc sec.3.4)")
	// Same position, not re-appended at the end: in step 3 the notice still
	// sits between tool_a's result and tool_b's call, where it landed at the
	// step-2 boundary. Revert-check performed: made spliceCarried append
	// carried splices at the end of base -- this assertion FAILED (notice
	// after call_b). Restored; re-ran, passed.
	noticeAt := strings.Index(bodies[2], "job output")
	callBAt := strings.Index(bodies[2], `"id":"call_b"`)
	require.NotEqual(t, -1, callBAt)
	require.Less(t, noticeAt, callBAt, "a carried notice must keep its original position in later steps")
}

// completionTriggerTool is a fantasy.AgentTool that, when store is set,
// transitions (owner, toolCallID) to completed as part of its own Run() --
// simulating an async job finishing WHILE a tool call is in flight, strictly
// between that tool's own call and its result.
type completionTriggerTool struct {
	name       string
	store      *session.AsyncJobStore
	owner      string
	toolCallID string
}

func (c *completionTriggerTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: c.name} }

func (c *completionTriggerTool) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if c.store != nil {
		if _, err := c.store.Transition(ctx, session.TransitionParams{
			Owner: c.owner, ToolCallID: c.toolCallID, State: "completed", ResultSummary: "job output", Wake: true,
		}); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	return fantasy.NewTextResponse(c.name + " done"), nil
}

func (c *completionTriggerTool) ProviderOptions() fantasy.ProviderOptions     { return nil }
func (c *completionTriggerTool) SetProviderOptions(_ fantasy.ProviderOptions) {}
