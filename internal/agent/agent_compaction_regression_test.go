package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func compactionRetryServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			payload := `data: {"id":"cut","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"abandoned reasoning","content":"abandoned text"},"finish_reason":null}]}` + "\n\n"
			_, err = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
			require.NoError(t, err)
			_, err = fmt.Fprintf(conn, "%x\r\n%s\r\n", len(payload), payload)
			require.NoError(t, err)
			require.NoError(t, conn.Close()) // incomplete chunked body produces io.ErrUnexpectedEOF.
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"final reasoning\",\"content\":\"final summary\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"ok\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func compactionFixture(t *testing.T, srv *httptest.Server) (*sessionAgent, string, Model) {
	t.Helper()
	env := testEnv(t)
	lm := languageModelFromServer(t, srv)
	model := Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}}
	a := NewSessionAgent(SessionAgentOptions{SmartModel: model, FastModel: model, SystemPrompt: "probe", IsYolo: true, Sessions: env.sessions, Messages: env.messages, Tools: []fantasy.AgentTool{}, DisableAutoSummarize: true}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "compaction regression")
	require.NoError(t, err)
	return a, sess.ID, a.smartModel.Get()
}

func TestSilentCompactionSummarizesAndDeletesAllNonPinnedHistory(t *testing.T) {
	srv := summarizeSSEServer(10, 2, nil)
	t.Cleanup(srv.Close)
	a, sid, model := compactionFixture(t, srv)
	var seeded []message.Message
	for i := 0; i < 9; i++ {
		m, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("history-%d", i)}}})
		require.NoError(t, err)
		seeded = append(seeded, m)
	}
	require.NoError(t, a.messages.SetPinned(t.Context(), seeded[2].ID, true))
	require.NoError(t, a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, ""))

	sess, err := a.sessions.Get(t.Context(), sid)
	require.NoError(t, err)
	require.NotEmpty(t, sess.SummaryMessageID)
	before, err := a.messages.List(t.Context(), sid)
	require.NoError(t, err)
	require.Len(t, before, 2)
	require.Equal(t, []string{seeded[2].ID, sess.SummaryMessageID}, []string{before[0].ID, before[1].ID})
	require.True(t, before[1].IsSummaryMessage)
	modelHistory, err := a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, []string{seeded[2].ID, sess.SummaryMessageID}, []string{modelHistory[0].ID, modelHistory[1].ID})
	require.Equal(t, message.User, modelHistory[1].Role)
	for i, m := range seeded {
		_, err = a.messages.Get(t.Context(), m.ID)
		if i == 2 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	_, err = a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "post-compaction"}}})
	require.NoError(t, err)
	history, err := a.messages.List(t.Context(), sid)
	require.NoError(t, err)
	require.Len(t, history, 3)
	require.Equal(t, []string{seeded[2].ID, sess.SummaryMessageID}, []string{history[0].ID, history[1].ID})
	require.Equal(t, "post-compaction", history[2].Content().Text)
	require.True(t, history[1].IsSummaryMessage)
	modelHistory, err = a.getSessionMessages(t.Context(), sess)
	require.NoError(t, err)
	require.Equal(t, []string{seeded[2].ID, sess.SummaryMessageID, history[2].ID}, []string{modelHistory[0].ID, modelHistory[1].ID, modelHistory[2].ID})
	require.Equal(t, message.User, modelHistory[1].Role)
}

func TestCompactionRetriesDiscardAbandonedSummaryText(t *testing.T) {
	for _, silent := range []bool{false, true} {
		name := "manual"
		if silent {
			name = "silent"
		}
		t.Run(name, func(t *testing.T) {
			srv, calls := compactionRetryServer(t)
			a, sid, model := compactionFixture(t, srv)
			for i := 0; i < 8; i++ {
				role := message.User
				if i%2 == 1 {
					role = message.Assistant
				}
				_, err := a.messages.Create(t.Context(), sid, message.CreateMessageParams{Role: role, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("old-%d", i)}}})
				require.NoError(t, err)
			}
			var err error
			if silent {
				err = a.runSummarizeSilent(t.Context(), sid, fantasy.ProviderOptions{}, model, "")
			} else {
				err = a.runSummarizeBody(t.Context(), sid, fantasy.ProviderOptions{}, model, "")
			}
			require.NoError(t, err)
			require.GreaterOrEqual(t, calls.Load(), int64(2))
			sess, err := a.sessions.Get(t.Context(), sid)
			require.NoError(t, err)
			summary, err := a.messages.Get(t.Context(), sess.SummaryMessageID)
			require.NoError(t, err)
			require.Equal(t, "final summary", summary.Content().Text)
			require.NotContains(t, summary.Content().Text, "abandoned")
			require.Equal(t, "final reasoning", summary.FullReasoningText())
			require.NotContains(t, summary.FullReasoningText(), "abandoned")
		})
	}
}
