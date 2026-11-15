package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func batchSSEToolCall(callID, name, args string) string {
	return fmt.Sprintf("data: %s\n\ndata: %s\n\ndata: [DONE]\n\n",
		fmt.Sprintf(`{"id":"q","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, callID, name, args),
		`{"id":"q","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
}

// questionProvider makes every delegated child ask one question, then finish
// with the answer it received.
func questionProvider() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &request)
		system, _ := "", ""
		if len(request.Messages) > 0 {
			system, _ = request.Messages[0].Content.(string)
		}
		if !strings.Contains(system, "an agent for Rush") {
			_, _ = io.WriteString(w, batchSSEText("ok"))
			return
		}
		answer := ""
		for _, message := range request.Messages {
			if message.Role == "tool" {
				answer, _ = message.Content.(string)
			}
		}
		if answer == "" {
			_, _ = io.WriteString(w, batchSSEToolCall("call-ask", "ask_question", `{"question":"Which port?","options":["8080"]}`))
			return
		}
		_, _ = io.WriteString(w, batchSSEText("child finished: "+answer))
	}
}

func (f *batchFixture) grantChildQuestionTools(t *testing.T) {
	t.Helper()
	task, ok := f.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok)
	tools := slices.Clone(task.AllowedTools)
	for _, name := range []string{"wakein", "ask_question"} {
		if !slices.Contains(tools, name) {
			tools = append(tools, name)
		}
	}
	f.cfg.UpdateAgentAllowedTools(config.AgentTask, tools)
}

func (f *batchFixture) waitAwaiting(t *testing.T, token, root string) (apiBatch, apiBatchNode) {
	t.Helper()
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		current := f.do(t, token, http.MethodGet, "/batches/"+root, nil).batch(t)
		for _, node := range current.Nodes {
			if node.AwaitingAnswer {
				return current, node
			}
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			require.FailNow(t, "no node ever awaited an answer", "state=%s", current.State)
		}
	}
}

func TestAPIBatchQuestion_AnswerContinuesExistingIncarnation(t *testing.T) {
	f := newBatchFixture(t, questionProvider())
	f.grantChildQuestionTools(t)
	token := f.token(t, "ask", []string{"read", "run"}, "ask", config.APITokenBinding{})
	stranger := f.token(t, "stranger", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "ask me something"}).accepted(t)

	_, held := f.waitAwaiting(t, token, accepted.RootID)
	require.Equal(t, "Which port?", held.Question)
	require.NotEmpty(t, held.ChildSessionID, "the held question names its child session")
	answerPath := "/batches/" + accepted.RootID + "/nodes/" + held.ID + "/answer"

	require.Equal(t, http.StatusNotFound, f.do(t, stranger, http.MethodPost, answerPath, map[string]any{"text": "9999"}).Status, "another token cannot answer")
	require.Equal(t, http.StatusBadRequest, f.do(t, token, http.MethodPost, answerPath, map[string]any{"text": " "}).Status)

	delivered := f.do(t, token, http.MethodPost, answerPath, map[string]any{"text": "8080"})
	require.Equal(t, http.StatusAccepted, delivered.Status, string(delivered.Body))

	done := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "completed", done.State, "the same execution continued after the answer")
	leaf := apiBatchNode{}
	for _, node := range done.Nodes {
		if node.Kind == "task" {
			leaf = node
		}
	}
	require.Equal(t, held.ChildSessionID, leaf.ChildSessionID, "no second child: the answer continued the existing one")
	require.False(t, leaf.AwaitingAnswer)

	// The question is stale now: a second answer is a 409, never a new generation.
	stale := f.do(t, token, http.MethodPost, answerPath, map[string]any{"text": "again"})
	require.Equal(t, http.StatusConflict, stale.Status, string(stale.Body))
}

func TestAPIBatchAnswerWithoutAQuestionIsAConflict(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "noq", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).accepted(t)
	done := f.waitBatchDone(t, token, accepted.RootID)
	var leaf apiBatchNode
	for _, node := range done.Nodes {
		if node.Kind == "task" {
			leaf = node
		}
	}
	resp := f.do(t, token, http.MethodPost, "/batches/"+accepted.RootID+"/nodes/"+leaf.ID+"/answer", map[string]any{"text": "x"})
	require.Equal(t, http.StatusConflict, resp.Status, string(resp.Body))
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodPost, "/batches/"+accepted.RootID+"/nodes/missing/answer", map[string]any{"text": "x"}).Status)
}
