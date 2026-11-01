package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type emptyReasoningRequestCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *emptyReasoningRequestCapture) add(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, body)
}

func (c *emptyReasoningRequestCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

func emptyReasoningReplayServer(t *testing.T) (*httptest.Server, *emptyReasoningRequestCapture) {
	t.Helper()
	capture := &emptyReasoningRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		capture.add(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"empty-reasoning-replay\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"empty-reasoning-replay\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}

func emptyReasoningReplayAgent(t *testing.T, srv *httptest.Server) (*sessionAgent, string, Model) {
	t.Helper()
	env := testEnv(t)
	lm := languageModelFromServer(t, srv)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
	a := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "probe", IsYolo: true,
		Sessions: env.sessions, Messages: env.messages, Tools: []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "empty reasoning replay")
	require.NoError(t, err)
	return a, sess.ID, model
}

func seedEmptyReasoningToolExchange(t *testing.T, a *sessionAgent, sessionID, provider string, withFollowingUser bool) {
	t.Helper()
	ctx := t.Context()
	_, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "earlier request"}},
	})
	require.NoError(t, err)
	assistant, err := a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.Assistant, Provider: provider,
		Parts: []message.ContentPart{
			message.ReasoningContent{},
			message.ToolCall{ID: "call_empty_reasoning", Name: "lookup", Input: `{}`},
		},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, a.messages.Update(ctx, assistant))
	_, err = a.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call_empty_reasoning", Name: "lookup", Content: "stored tool result"}},
	})
	require.NoError(t, err)
	if withFollowingUser {
		_, err = a.messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "following stored user"}},
		})
		require.NoError(t, err)
	}
}

func requireEmptyReasoningToolReplay(t *testing.T, body string, wantFollowingUser bool) {
	t.Helper()
	// These are wire assertions on the OpenAI-compatible request, not just
	// fantasy.Message assertions: inspect the assistant message that owns the
	// tool call so a separate message cannot satisfy the reasoning assertion.
	var request struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	var assistant map[string]json.RawMessage
	for _, msg := range request.Messages {
		var calls []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(msg["tool_calls"], &calls); err == nil {
			for _, call := range calls {
				if call.ID == "call_empty_reasoning" {
					assistant = msg
				}
			}
		}
	}
	require.NotNil(t, assistant, "assistant message with tool call")
	var reasoning string
	require.Contains(t, assistant, "reasoning_content")
	require.NoError(t, json.Unmarshal(assistant["reasoning_content"], &reasoning))
	require.Empty(t, reasoning)
	require.Contains(t, body, "stored tool result")
	if wantFollowingUser {
		require.Contains(t, body, "following stored user")
	}
}

func requireNoEmptyReasoningOnToolReplay(t *testing.T, body string) {
	t.Helper()
	var request struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	var assistant map[string]json.RawMessage
	for _, msg := range request.Messages {
		var calls []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(msg["tool_calls"], &calls); err == nil {
			for _, call := range calls {
				if call.ID == "call_empty_reasoning" {
					assistant = msg
				}
			}
		}
	}
	require.NotNil(t, assistant, "assistant message with tool call")
	require.NotContains(t, assistant, "reasoning_content")
}

func TestTurnReplaysEmptyOpenAICompatReasoningWithToolExchange(t *testing.T) {
	srv, capture := emptyReasoningReplayServer(t)
	a, sid, _ := emptyReasoningReplayAgent(t, srv)
	seedEmptyReasoningToolExchange(t, a, sid, openaicompat.Name, true)

	_, err := a.Run(t.Context(), SessionAgentCall{SessionID: sid, Prompt: "new user turn", MaxOutputTokens: 100})
	require.NoError(t, err)
	requests := capture.snapshot()
	require.NotEmpty(t, requests)
	requireEmptyReasoningToolReplay(t, requests[0], true)
	require.Contains(t, requests[0], "new user turn")
}

func TestTurnDoesNotReplayEmptyReasoningFromOtherProvider(t *testing.T) {
	srv, capture := emptyReasoningReplayServer(t)
	a, sid, _ := emptyReasoningReplayAgent(t, srv)
	// The request target is still OpenAI-compatible; the stored assistant
	// message's provider provenance is what distinguishes this control.
	seedEmptyReasoningToolExchange(t, a, sid, anthropic.Name, true)

	_, err := a.Run(t.Context(), SessionAgentCall{SessionID: sid, Prompt: "new user turn", MaxOutputTokens: 100})
	require.NoError(t, err)
	requests := capture.snapshot()
	require.NotEmpty(t, requests)
	body := requests[0]
	requireNoEmptyReasoningOnToolReplay(t, body)
	require.Contains(t, body, "stored tool result")
	require.Contains(t, body, "following stored user")
}

func TestCompactionReplaysEmptyOpenAICompatReasoning(t *testing.T) {
	for _, silent := range []bool{false, true} {
		name := "manual"
		if silent {
			name = "silent"
		}
		t.Run(name, func(t *testing.T) {
			srv, capture := emptyReasoningReplayServer(t)
			a, sid, model := emptyReasoningReplayAgent(t, srv)
			seedEmptyReasoningToolExchange(t, a, sid, openaicompat.Name, false)
			if silent {
				// runSummarizeSilent needs enough messages and summarizes the
				// older prefix. Add five tail messages so the exchange remains
				// inside that prefix, before the first eligible non-tool message.
				for i, role := range []message.MessageRole{message.User, message.Assistant, message.User, message.Assistant, message.User} {
					parts := []message.ContentPart{message.TextContent{Text: fmt.Sprintf("silent tail %d", i)}}
					m, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: parts})
					require.NoError(t, err)
					if role == message.Assistant {
						m.AddFinish(message.FinishReasonEndTurn, "", "")
						require.NoError(t, a.messages.Update(t.Context(), m))
					}
				}
				require.NoError(t, a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))
			} else {
				require.NoError(t, a.runSummarizeBody(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))
			}
			requests := capture.snapshot()
			require.Len(t, requests, 1)
			requireEmptyReasoningToolReplay(t, requests[0], false)
			require.Contains(t, requests[0], "earlier request")
		})
	}
}

type openAICompatStallRetryModel struct {
	*stallRetryToolModel
}

func (*openAICompatStallRetryModel) Provider() string { return openaicompat.Name }

func TestRunTurnStallRetryKeepsEmptyReasoningToolCall(t *testing.T) {
	env := testEnv(t)
	t.Cleanup(stallDumpWrites.Wait)
	baseModel := &stallRetryToolModel{second: make(chan struct{}), third: make(chan struct{})}
	model := &openAICompatStallRetryModel{stallRetryToolModel: baseModel}
	tool := fantasy.NewAgentTool("side_effect", "count side effect", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("side-effect-result"), nil
	})
	sa := testSessionAgent(env, model, model, "test", tool).(*sessionAgent)
	sa.SetTimeoutOptions(false, time.Hour)
	sa.streamWatchdogTick = time.Hour
	session, err := env.sessions.Create(t.Context(), "Existing Session")
	require.NoError(t, err)
	seedEmptyReasoningToolExchange(t, sa, session.ID, openaicompat.Name, false)

	// The fake clock and channels drive the retry; the test uses no sleep.
	t0 := time.Now()
	setNow := installFakeStallNow(t)
	setNow(t0)
	done := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: session.ID, Prompt: "run it", MaxOutputTokens: 100,
			CallOptions: &CallOptions{TurnStallTimeout: time.Second},
		})
		done <- runErr
	}()

	<-baseModel.second
	clockValue, ok := armedStallClocks.Load(session.ID)
	require.True(t, ok)
	setNow(t0.Add(11 * time.Minute))
	stallSamplerCheck(session.ID, clockValue.(*stallClock), stallNow())
	<-baseModel.third
	require.NoError(t, <-done)

	baseModel.mu.Lock()
	requests := append([]fantasy.Prompt(nil), baseModel.requests...)
	baseModel.mu.Unlock()
	require.Len(t, requests, 3)

	var replayedAssistant fantasy.Message
	found := false
	for _, msg := range requests[2] {
		if msg.Role != fantasy.MessageRoleAssistant {
			continue
		}
		hasReasoning := false
		hasToolCall := false
		for _, part := range msg.Content {
			if reasoning, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok && reasoning.Text == "" {
				hasReasoning = true
			}
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && call.ToolCallID == "call_empty_reasoning" {
				hasToolCall = true
			}
		}
		if hasReasoning && hasToolCall {
			replayedAssistant = msg
			found = true
			break
		}
	}
	require.True(t, found, "retry prompt assistant message should contain the empty reasoning and stored tool call; message: %+v", replayedAssistant)
}
