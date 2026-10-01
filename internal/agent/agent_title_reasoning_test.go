// Tests for the GLM title-generation path: request-level thinking control
// (z.ai ignores the prompt-level /no_think tricks, so the request must carry
// extra_body.thinking), and the reasoning-stream fallback — used only when
// reasoning COMPLETED (a length-truncated stream ends mid-thought), with
// prefix/quote/length sanitisation applied to the recovered line.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
)

// TestTitleFromReasoning pins the extraction rule: the last non-empty line
// of the reasoning stream is the raw title candidate.
func TestTitleFromReasoning(t *testing.T) {
	tests := []struct {
		name      string
		reasoning string
		want      string
	}{
		{"final line wins", "let me think\nuser asks about proxy\nSetup Tor socks5 proxy", "Setup Tor socks5 proxy"},
		{"trailing blank lines skipped", "thinking\nThe Answer\n  \n\t\n", "The Answer"},
		{"single line", "Only Title", "Only Title"},
		{"empty", "", ""},
		{"whitespace only", "  \n \n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, titleFromReasoning(tc.reasoning))
		})
	}
}

// TestSanitizeReasoningTitle pins the sanitisation applied to a title
// recovered from a reasoning stream: answer prefixes and wrapping quotes are
// stripped, and the line is capped at titleMaxReasoningLen (cut at a word
// boundary where possible).
func TestSanitizeReasoningTitle(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "Setup Tor socks5 proxy", "Setup Tor socks5 proxy"},
		{"title prefix stripped", `Title: "My Great Title"`, "My Great Title"},
		{"cyrillic prefix stripped", "Заголовок: Прокси через Tor", "Прокси через Tor"},
		{"dash prefix stripped", "Title - My Great Title", "My Great Title"},
		{"quotes stripped", "«Wrapped Title»", "Wrapped Title"},
		{
			"long line cut at word boundary",
			"A very long reasoning answer that just keeps going and going well past the eighty character mark and beyond",
			"A very long reasoning answer that just keeps going and going well past the",
		},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeReasoningTitle(tc.raw)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len([]rune(got)), titleMaxReasoningLen)
		})
	}
}

// mustJSON marshals v or panics; SSE fixtures are static test data, a marshal
// failure there is a bug in the test itself.
func mustJSON(v any) string {
	d, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(d)
}

// trimBraces strips the outer braces of a JSON object so its fields can be
// spliced into a larger object literal.
func trimBraces(s string) string {
	return strings.TrimPrefix(strings.TrimSuffix(s, "}"), "{")
}

// titleTestServer returns a title SSE server that decodes each request body
// into bodyCh and answers with a single reasoning_content delta carrying
// reasoning (an optional content text delta when content is non-empty),
// then finish_reason.
func titleTestServer(t *testing.T, bodyCh chan<- map[string]any, reasoning, content, finish string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		bodyCh <- body

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		delta := map[string]any{}
		if reasoning != "" {
			delta["reasoning_content"] = reasoning
		}
		if content != "" {
			delta["content"] = content
		}
		deltaJSON := mustJSON(delta)

		fin := any(nil)
		if finish != "" {
			fin = finish
		}
		chunks := []string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant",%s},"finish_reason":null}]}`, trimBraces(deltaJSON)),
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":%s}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}`, mustJSON(fin)),
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
}

// runTitleProbe drives one Run() with the given fast model config and title
// server, and returns the session title that landed.
func runTitleProbe(t *testing.T, fastCfg Model, titleSrv *httptest.Server) string {
	t.Helper()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	srv := singleTurnSSEServer(nil)
	t.Cleanup(srv.Close)
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	fastCfg.Model = titleLM
	a := NewSessionAgent(SessionAgentOptions{
		SmartModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		FastModel:            fastCfg,
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})

	_, err = a.Run(context.Background(), SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "first message",
		MaxOutputTokens: 1000,
	})
	require.NoError(t, err)

	updated, err := env.sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	return updated.Title
}

// zaiFastModel is a fast model whose CONFIGURED provider ID is z.ai — the
// same domain getProviderOptions' ZAI switch classifies on.
func zaiFastModel() Model {
	return Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Provider: string(catwalk.InferenceProviderZAI)},
	}
}

// TestRun_TitleThinkingDisabledForZAI: the title request to a z.ai-configured
// fast model must carry a thinking={type:disabled} option on the wire —
// z.ai ignores the prompt-level "/no_think" trick, and without the request
// option the whole answer streams as reasoning_content.
func TestRun_TitleThinkingDisabledForZAI(t *testing.T) {
	bodyCh := make(chan map[string]any, 4)
	titleSrv := titleTestServer(t, bodyCh, "thinking hard\nReasoning Answer Line", "", "stop")
	t.Cleanup(titleSrv.Close)

	title := runTitleProbe(t, zaiFastModel(), titleSrv)
	assert.Equal(t, "Reasoning Answer Line", title)

	body := <-bodyCh
	thinking, ok := body["thinking"].(map[string]any)
	require.True(t, ok, "z.ai title request must carry a thinking option, got body: %v", body)
	assert.Equal(t, "disabled", thinking["type"])
}

// TestRun_TitleNoThinkingOptionForOtherProvider: a non-z.ai openai-compat
// provider keeps its default behavior — no thinking option on the title
// request at all.
func TestRun_TitleNoThinkingOptionForOtherProvider(t *testing.T) {
	bodyCh := make(chan map[string]any, 4)
	titleSrv := titleTestServer(t, bodyCh, "thinking hard\nOther Answer Line", "", "stop")
	t.Cleanup(titleSrv.Close)

	fastModel := Model{
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
		ModelCfg:   config.SelectedModel{Provider: "some-other-compat"},
	}
	title := runTitleProbe(t, fastModel, titleSrv)
	assert.Equal(t, "Other Answer Line", title)

	body := <-bodyCh
	assert.NotContains(t, body, "thinking",
		"non-z.ai providers must keep their default behavior: no thinking option on the title request")
}

// TestRun_TitleReasoningStopSanitized: reasoning-only + COMPLETED reasoning
// (finish stop) → the title is recovered from the reasoning stream with
// prefix/quote sanitisation.
func TestRun_TitleReasoningStopSanitized(t *testing.T) {
	bodyCh := make(chan map[string]any, 4)
	titleSrv := titleTestServer(t, bodyCh, "I will craft it now.\nTitle: \"Sanitized Reasoning Title\"", "", "stop")
	t.Cleanup(titleSrv.Close)

	title := runTitleProbe(t, zaiFastModel(), titleSrv)
	assert.Equal(t, "Sanitized Reasoning Title", title)
}

// TestRun_TitleFromReasoningOnlyResponse: a length-truncated reasoning-only
// response ends MID-THOUGHT, so its last line is a fragment, not an answer —
// the reasoning fallback must NOT fire and the attempt must fall through to
// the smart model instead.
func TestRun_TitleFromReasoningOnlyResponse(t *testing.T) {
	bodyCh := make(chan map[string]any, 4)
	titleSrv := titleTestServer(t, bodyCh, "maybe the title is Truncated Fragment Ti", "", "length")
	t.Cleanup(titleSrv.Close)

	title := runTitleProbe(t, zaiFastModel(), titleSrv)
	assert.Equal(t, "ok", title,
		"truncated reasoning must not become the title: the attempt must fall through to the smart model")
}
