package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const codexWSOverflowBodyLimit = 60000

// codexWSLocalManager redirects the fixed Codex endpoint to a local peer.
type codexWSLocalManager struct {
	*codexWebSocketTransport
	endpoint string
}

func (m codexWSLocalManager) Acquire(ctx context.Context, request codexprovider.WebSocketRequest) (codexprovider.WebSocketLease, error) {
	request.Endpoint = m.endpoint
	return m.codexWebSocketTransport.Acquire(ctx, request)
}

type codexWSStaticSource struct{}

func (codexWSStaticSource) Current() (oauth.Token, bool) {
	return oauth.Token{AccessToken: "local-fixture-token", AccountID: "local-account"}, true
}

func (codexWSStaticSource) Refresh(context.Context, oauth.Token) error { return nil }

type codexWSPeer struct {
	server   *httptest.Server
	mu       sync.Mutex
	frames   [][]byte
	upgrades atomic.Int32
	requests atomic.Int32
	http     atomic.Int32
}

func (p *codexWSPeer) frame(i int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.frames) {
		return nil
	}
	return p.frames[i]
}

// newCodexWSPeer replies to a frame via respond(frame, n); n is the 1-based
// request number across all connections, bounded at four requests.
func newCodexWSPeer(t *testing.T, respond func(frame []byte, n int) []string) *codexWSPeer {
	t.Helper()
	peer := &codexWSPeer{}
	peer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			peer.http.Add(1)
			http.Error(w, "unexpected HTTP inference", http.StatusBadGateway)
			return
		}
		peer.upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			n := int(peer.requests.Add(1))
			peer.mu.Lock()
			peer.frames = append(peer.frames, append([]byte(nil), frame...))
			peer.mu.Unlock()
			events := respond(frame, n)
			if n > 4 {
				events = []string{`{"type":"error","error":{"code":"fixture_unexpected_extra_request","message":"bounded fixture ceiling"}}`}
			}
			for _, event := range events {
				if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(peer.server.Close)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("codex ws peer: upgrades=%d requests=%d http=%d", peer.upgrades.Load(), peer.requests.Load(), peer.http.Load())
		}
	})
	return peer
}

func (p *codexWSPeer) model(t *testing.T) fantasy.LanguageModel {
	t.Helper()
	manager := nettransport.NewResponsesWSManager(t.Context(), 2)
	t.Cleanup(func() { _ = manager.Close() })
	policy := codexprovider.WebSocketPolicy{Transport: codexprovider.WebSocketTransportAuto}
	network := nettransport.ResolveNetworkConfig(nil, &config.NetworkConfig{Proxy: config.ProxyNone})
	transport := &codexWebSocketTransport{ResponsesWSManager: manager, current: func() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
		return policy, network, nil
	}}
	failHTTP := roundTripFunc(func(*http.Request) (*http.Response, error) {
		p.http.Add(1)
		return nil, fmt.Errorf("unexpected HTTP inference")
	})
	provider, err := codexprovider.NewWithSource(&http.Client{Transport: failHTTP}, codexWSStaticSource{}, codexprovider.WebSocketOptions{
		Manager:     codexWSLocalManager{codexWebSocketTransport: transport, endpoint: strings.Replace(p.server.URL, "http://", "ws://", 1)},
		Current:     func() (codexprovider.WebSocketPolicy, error) { return policy, nil },
		EncodeFrame: nettransport.ResponsesWSCodexFullFrame,
	})
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	return model
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func codexWSReply(text string) []string {
	payload, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
	return []string{string(payload), `{"type":"response.completed","response":{"id":"resp-ok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"continued after persisted summary"}]}],"usage":{"input_tokens":20,"output_tokens":8,"total_tokens":28}}}`}
}

const codexWSLifecycle = `{"type":"response.in_progress","response":{"id":"resp-overflow","object":"response","status":"in_progress","output":[],"usage":null}}`

var codexWSRejections = []struct{ name, event string }{
	{"response.failed nested response error", `{"type":"response.failed","response":{"id":"resp-overflow","status":"failed","output":[],"usage":null,"error":{"code":"context_length_exceeded","message":"request exceeds context"}}}`},
	{"error nested error", `{"type":"error","error":{"code":"context_length_exceeded","message":"request exceeds context"}}`},
	{"error flat fields", `{"type":"error","code":"context_length_exceeded","message":"request exceeds context"}`},
}

// runCodexWSOverflow drives one oversized Codex WebSocket turn through the
// real coordinator and returns the run error plus the worker request count.
func runCodexWSOverflow(t *testing.T, peer *codexWSPeer) (workers int32, runErr error) {
	t.Helper()
	f, _, _ := newCodexOverflowRun(t, "codex-ws-overflow", nil, func(int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unused legacy handler", http.StatusTeapot)
		}
	})
	f.sa.disableAutoSummarize = false
	var workerCount atomic.Int32
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		workerCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Persisted summary of earlier context.\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":12,\"total_tokens\":112}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(workerServer.Close)

	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir()},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Models:    make(map[config.SelectedModelType]config.SelectedModel),
	}, config.IdentityResolver())
	codexCfg := config.ProviderConfig{ID: "openai-codex", Type: "openai-compat", OAuthToken: &oauth.Token{AccessToken: "local-fixture-token"}, Models: []catwalk.Model{{ID: "gpt-5-codex", ContextWindow: 200000, DefaultMaxTokens: 1000}}}
	workerCfg := config.ProviderConfig{ID: "overflow-worker", Type: "openai-compat", BaseURL: workerServer.URL + "/v1", APIKey: "worker-fixture-token", Network: &config.NetworkConfig{Proxy: config.ProxyNone}, Models: []catwalk.Model{{ID: "default-worker", ContextWindow: 200000, DefaultMaxTokens: 1000}}}
	store.Config().Providers.Set(codexCfg.ID, codexCfg)
	store.Config().Providers.Set(workerCfg.ID, workerCfg)
	store.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: workerCfg.ID, Model: "default-worker"}
	store.Config().SetupAgents()
	f.coord.cfg, f.sa.config = store, store
	f.coord.skillTracker = skills.NewTracker(nil)
	f.coord.currentAgent = f.sa
	model := Model{Model: peer.model(t), CatwalkCfg: codexCfg.Models[0], ModelCfg: store.Config().Models[config.SelectedModelTypeSmart]}
	f.sa.SetModels(model, model)
	pinned := &resolvedOverrides{smart: model, fast: model, providerCfg: codexCfg}

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	ctx = WithCallOptions(ctx, &CallOptions{WebTurnOptIn: true})
	rows, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var pinID string
	for _, row := range rows {
		if strings.Contains(row.FullText(), "pinned authority: preserve this scope") {
			pinID = row.ID
		}
	}
	require.NotEmpty(t, pinID)
	for i := 0; i < 42; i++ {
		old := fmt.Sprintf("old persisted context %02d %s", i, strings.Repeat("historical-context-", 55))
		_, err = f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: old}}})
		require.NoError(t, err)
		_, err = f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "historical answer " + strings.Repeat("answer-context-", 30)}}})
		require.NoError(t, err)
	}
	require.NoError(t, f.env.messages.SetPinned(ctx, pinID, true))
	_, runErr = f.coord.runInternal(ctx, f.sessID, "Summarize the useful details and continue the current task.", pinned)
	return workerCount.Load(), runErr
}

func TestCodexWebSocketContextRejectionRecoversOnceWithFullFrame(t *testing.T) {
	for _, rejection := range codexWSRejections {
		t.Run(rejection.name, func(t *testing.T) {
			peer := newCodexWSPeer(t, func(frame []byte, _ int) []string {
				if len(frame) > codexWSOverflowBodyLimit {
					return []string{codexWSLifecycle, rejection.event}
				}
				return codexWSReply("continued after persisted summary")
			})
			workers, runErr := runCodexWSOverflow(t, peer)
			require.NoError(t, runErr, "ws requests=%d upgrades=%d workers=%d", peer.requests.Load(), peer.upgrades.Load(), workers)
			require.EqualValues(t, 1, workers, "automatic recovery must run exactly once")
			require.EqualValues(t, 2, peer.requests.Load(), "oversized request plus one continuation")
			require.EqualValues(t, 2, peer.upgrades.Load(), "the rejected connection must be retired")
			require.Zero(t, peer.http.Load())
			rejected, continuation := peer.frame(0), peer.frame(1)
			require.Greater(t, len(rejected), codexWSOverflowBodyLimit)
			require.Less(t, len(continuation), codexWSOverflowBodyLimit)
			require.NotContains(t, string(continuation), "previous_response_id", "the continuation must be a full frame")
			require.Contains(t, string(continuation), "Persisted summary of earlier context")
		})
	}
}

func TestCodexWebSocketContextRejectionAfterOutputDoesNotRecover(t *testing.T) {
	peer := newCodexWSPeer(t, func(frame []byte, _ int) []string {
		if len(frame) > codexWSOverflowBodyLimit {
			return []string{`{"type":"response.output_text.delta","delta":"partial accepted text"}`, codexWSRejections[1].event}
		}
		return codexWSReply("must not be reached")
	})
	workers, runErr := runCodexWSOverflow(t, peer)
	require.Error(t, runErr)
	require.Zero(t, workers, "a post-output rejection must not trigger summarization")
	require.EqualValues(t, 1, peer.requests.Load(), "a post-output rejection must not be replayed")
	require.EqualValues(t, 1, peer.upgrades.Load())
	require.Zero(t, peer.http.Load())
}

func TestCodexWebSocketProviderFailureClassifiesLikeSSE(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	for _, tc := range []struct {
		name  string
		event string
		hard  bool
	}{
		{"usage limit reached", `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`, true},
		{"rate limit", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}}`, false},
		{"server overloaded", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newCodexWSPeer(t, func([]byte, int) []string { return []string{tc.event} })
			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "ws-classify"), 5*time.Second)
			defer cancel()
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}}
			_, wsErr := peer.model(t).Generate(ctx, call)
			require.Error(t, wsErr)

			sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.event)
			}))
			t.Cleanup(sse.Close)
			target, err := url.Parse(sse.URL)
			require.NoError(t, err)
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.Proxy = nil
			provider, err := codexprovider.New(&http.Client{Transport: overflowRecoveryRT{target: target, base: base}, Timeout: 3 * time.Second}, &oauth.Token{AccessToken: "local-fixture-token"})
			require.NoError(t, err)
			sseModel, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
			require.NoError(t, err)
			_, sseErr := sseModel.Generate(ctx, call)
			require.Error(t, sseErr)

			var wsProvider, sseProvider *fantasy.ProviderError
			require.ErrorAs(t, wsErr, &wsProvider)
			require.ErrorAs(t, sseErr, &sseProvider)
			require.Equal(t, tc.hard, IsHardQuotaLimit(sseErr), "SSE reference classification")
			require.Equal(t, IsHardQuotaLimit(sseErr), IsHardQuotaLimit(wsErr), "WebSocket must classify a hard limit like SSE")
			require.Equal(t, sseProvider.IsContextTooLarge(), wsProvider.IsContextTooLarge())
			require.True(t, responsesWSReplayForbidden(wsErr), "a sent WebSocket request must stay non-replayable")
			require.Equal(t, classTerminal, classifyProviderError(wsErr))
			require.EqualValues(t, 1, peer.requests.Load())
			require.Zero(t, peer.http.Load())
		})
	}
}
