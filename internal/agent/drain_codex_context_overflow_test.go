package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/stretchr/testify/require"
)

type overflowCodexTransport struct {
	target *url.URL
	server *httptest.Server
}

type overflowCountTool struct {
	calls atomic.Int32
}

type overflowDiagnostics struct {
	mu             sync.Mutex
	requests       int
	methods        map[string]int
	paths          map[string]int
	eventKinds     map[string]int
	requestedModel string
	observedModel  string
	pinSeen        int
}

type overflowDiagnosticWriter struct {
	http.ResponseWriter
	diagnostics *overflowDiagnostics
	seen        int
}

func (t *overflowCountTool) Info() fantasy.ToolInfo                     { return fantasy.ToolInfo{Name: "overflow_probe"} }
func (t *overflowCountTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *overflowCountTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *overflowCountTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.calls.Add(1)
	return fantasy.NewTextResponse("tool executed"), nil
}

func (rt overflowCodexTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	copyURL := *request.URL
	forwarded.URL = &copyURL
	forwarded.URL.Scheme = rt.target.Scheme
	forwarded.URL.Host = rt.target.Host
	forwarded.Host = rt.target.Host
	return rt.server.Client().Transport.RoundTrip(forwarded)
}

func (w *overflowDiagnosticWriter) Write(p []byte) (int, error) {
	const maxDiagnosticBytes = 16 << 10
	if w.seen < maxDiagnosticBytes {
		remaining := maxDiagnosticBytes - w.seen
		chunk := p
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		w.seen += len(chunk)
		w.diagnostics.recordEventKinds(chunk)
	}
	return w.ResponseWriter.Write(p)
}

func (d *overflowDiagnostics) recordRequest(r *http.Request) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++
	if d.methods == nil {
		d.methods = make(map[string]int)
		d.paths = make(map[string]int)
		d.eventKinds = make(map[string]int)
	}
	d.methods[r.Method]++
	d.paths[r.URL.Path]++
	return d.requests
}

func (d *overflowDiagnostics) recordRequestMetadata(body []byte) {
	const maxPinOccurrences = 4
	var request struct {
		Model string `json:"model"`
		Input []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if request.Model != "" {
		d.observedModel = request.Model
	}
	for _, item := range request.Input {
		for _, part := range item.Content {
			if strings.Contains(part.Text, "pinned authority: preserve this scope") {
				if d.pinSeen < maxPinOccurrences {
					d.pinSeen++
				}
			}
		}
	}
}

func (d *overflowDiagnostics) recordEventKinds(data []byte) {
	const maxEventKinds = 12
	for len(data) > 0 {
		start := strings.Index(string(data), `"type":"`)
		if start < 0 {
			return
		}
		data = data[start+len(`"type":"`):]
		end := strings.IndexByte(string(data), '"')
		if end <= 0 {
			return
		}
		kind := string(data[:end])
		d.mu.Lock()
		if _, exists := d.eventKinds[kind]; exists || len(d.eventKinds) < maxEventKinds {
			d.eventKinds[kind]++
		}
		d.mu.Unlock()
		data = data[end+1:]
	}
}

func (d *overflowDiagnostics) failReport(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Errorf("bounded Codex fixture diagnostics: requested_model=%s observed_model=%s pinned_input_occurrences=%d requests=%d methods=%v paths=%v event_kinds=%v", d.requestedModel, d.observedModel, d.pinSeen, d.requests, d.methods, d.paths, d.eventKinds)
	})
}

func newCodexOverflowRun(t *testing.T, title string, tools []fantasy.AgentTool, handler func(request int) http.HandlerFunc) (*attemptFixture, *resolvedOverrides, *overflowDiagnostics) {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	diagnostics := &overflowDiagnostics{requestedModel: "gpt-5-codex"}
	diagnostics.failReport(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := diagnostics.recordRequest(r)
		if request > 2 {
			http.Error(w, "bounded Codex fixture request ceiling", http.StatusPaymentRequired)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		diagnostics.recordRequestMetadata(body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		writer := &overflowDiagnosticWriter{ResponseWriter: w, diagnostics: diagnostics}
		handler(request).ServeHTTP(writer, r)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	provider, err := codexprovider.New(&http.Client{
		Transport: overflowCodexTransport{target: target, server: server},
		Timeout:   3 * time.Second,
	}, &oauth.Token{AccessToken: "fixture-token"})
	require.NoError(t, err)
	languageModel, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	f := newAttemptFixture(t, title, attemptFixtureOpts{noIdle: true, tools: tools})
	cfg, err := config.Init(f.env.workingDir, "", false)
	require.NoError(t, err)
	providerConfig := config.ProviderConfig{ID: codexprovider.Name, Type: "openai-codex", OAuthToken: &oauth.Token{AccessToken: "fixture-token"}, Models: []catwalk.Model{{ID: "gpt-5-codex", Name: "Codex", ContextWindow: 200000, DefaultMaxTokens: 1000}}}
	cfg.Config().Providers.Set(codexprovider.Name, providerConfig)
	selected := config.SelectedModel{Provider: codexprovider.Name, Model: "gpt-5-codex"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = selected
	cfg.Config().Models[config.SelectedModelTypeFast] = selected
	f.coord.cfg = cfg
	f.coord.sessions, f.coord.messages, f.coord.permissions = f.env.sessions, f.env.messages, f.env.permissions
	f.coord.currentAgent = f.sa
	f.coord.skillTracker = skills.NewTracker(nil)
	model := Model{Model: languageModel, CatwalkCfg: providerConfig.Models[0], ModelCfg: selected}
	f.model = model
	f.sa.SetModels(model, model)
	pinned := &resolvedOverrides{smart: model, fast: model, providerCfg: providerConfig}

	_, err = f.env.messages.Create(t.Context(), f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Earlier request: return a concise report about the scheduled update."}},
	})
	require.NoError(t, err)
	_, err = f.env.messages.Create(t.Context(), f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "pinned authority: preserve this scope"}},
	})
	require.NoError(t, err)
	messages, err := f.env.messages.List(t.Context(), f.sessID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	var pinnedAuthority message.Message
	for _, stored := range messages {
		if strings.Contains(stored.FullText(), "pinned authority: preserve this scope") {
			pinnedAuthority = stored
		}
	}
	require.NotEmpty(t, pinnedAuthority.ID, "authority fixture row must be found")
	require.NoError(t, f.env.messages.SetPinned(t.Context(), pinnedAuthority.ID, true))
	return f, pinned, diagnostics
}

func TestDrainCodexStructuredContextOverflowIsTerminalAndPreservesHistory(t *testing.T) {
	f, pinned, diagnostics := newCodexOverflowRun(t, "codex-overflow-drain", nil, func(int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial accepted text\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"type\":\"invalid_request_error\",\"message\":\"request exceeds context\"}}}\n\n")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	f.seedDebt(ctx, "call-codex-overflow", false)
	_, err := f.coord.runInternal(WithDrainCall(ctx), f.sessID, "", pinned)
	require.Error(t, err, "requests=%d", diagnostics.requestCount())
	require.EqualValues(t, 1, diagnostics.requestCount(), "terminal overflow must not replay; requests=%d", diagnostics.requestCount())
	require.Equal(t, "gpt-5-codex", diagnostics.observedModelID(), "selected model in provider request; requests=%d", diagnostics.requestCount())
	require.EqualValues(t, 1, diagnostics.pinCount(), "pinned authority in provider request; pin_occurrences=%d requests=%d", diagnostics.pinCount(), diagnostics.requestCount())
	require.EqualValues(t, 1, f.row(ctx, "call-codex-overflow").ReactedFailed)
	marker := f.wakeFailedMarkerText(ctx)
	require.Contains(t, marker, "Сожмите", "requests=%d", diagnostics.requestCount())
	require.NotContains(t, marker, "продолжение — при следующем ходе", "requests=%d", diagnostics.requestCount())

	messages, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var sawAuthority, pinnedAuthority, sawPartial bool
	for _, stored := range messages {
		if strings.Contains(stored.FullText(), "pinned authority: preserve this scope") {
			sawAuthority = true
			pinnedAuthority = stored.Pinned
		}
		sawPartial = sawPartial || strings.Contains(stored.FullText(), "partial accepted text")
	}
	require.True(t, sawAuthority, "history and pinned authority must survive; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
	require.True(t, pinnedAuthority, "authority message must remain pinned; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
	require.True(t, sawPartial, "partial output must survive the terminal failure; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
}

func TestCodexOverflowAfterAcceptedToolDoesNotReplayOrTruncateHistory(t *testing.T) {
	tool := &overflowCountTool{}
	f, pinned, diagnostics := newCodexOverflowRun(t, "codex-overflow-after-tool", []fantasy.AgentTool{tool}, func(request int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			if request == 1 {
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"id\":\"fc_item_1\",\"type\":\"function_call\",\"call_id\":\"call_tool_1\",\"name\":\"overflow_probe\",\"arguments\":\"{}\"}]}}\n\n")
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial accepted text\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"request exceeds context\"}}\n\n")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err := f.coord.runInternal(ctx, f.sessID, "Explain the scheduled update briefly.", pinned)
	require.Error(t, err, "requests=%d tool_calls=%d", diagnostics.requestCount(), tool.calls.Load())
	require.EqualValues(t, 2, diagnostics.requestCount(), "accepted tool turn followed by one overflow; requests=%d tool_calls=%d", diagnostics.requestCount(), tool.calls.Load())
	require.Equal(t, "gpt-5-codex", diagnostics.observedModelID(), "selected model in provider request; requests=%d", diagnostics.requestCount())
	require.EqualValues(t, 2, diagnostics.pinCount(), "pinned authority in each provider request; pin_occurrences=%d requests=%d", diagnostics.pinCount(), diagnostics.requestCount())
	require.EqualValues(t, 1, tool.calls.Load(), "accepted tool execution must not replay; requests=%d", diagnostics.requestCount())
	messages, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var sawAcceptedCall, sawToolResult, sawPartial, sawAuthority, pinnedAuthority bool
	for _, stored := range messages {
		if strings.Contains(stored.FullText(), "pinned authority: preserve this scope") {
			sawAuthority = true
			pinnedAuthority = stored.Pinned
		}
		sawPartial = sawPartial || strings.Contains(stored.FullText(), "partial accepted text")
		for _, call := range stored.ToolCalls() {
			sawAcceptedCall = sawAcceptedCall || call.ID == "call_tool_1" && call.Name == "overflow_probe"
		}
		for _, result := range stored.ToolResults() {
			sawToolResult = sawToolResult || result.ToolCallID == "call_tool_1" && result.Content == "tool executed"
		}
	}
	require.True(t, sawAcceptedCall, "accepted tool call remains; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
	require.True(t, sawToolResult, "accepted tool result remains; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
	require.True(t, sawPartial, "partial assistant output remains; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
	require.True(t, sawAuthority && pinnedAuthority, "pinned authority remains; requests=%d messages=%d", diagnostics.requestCount(), len(messages))
}

func (d *overflowDiagnostics) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests
}

func (d *overflowDiagnostics) pinCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pinSeen
}

func (d *overflowDiagnostics) observedModelID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.observedModel
}
