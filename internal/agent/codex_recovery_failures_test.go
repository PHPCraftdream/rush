package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/stretchr/testify/require"
)

const recoveryFailureCodexBody = `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"request exceeds context"}}`

type recoveryFailureTransport struct {
	target *url.URL
	base   http.RoundTripper
}

type recoveryNoReplayError struct {
	err error
}

func (e *recoveryNoReplayError) Error() string  { return e.err.Error() }
func (e *recoveryNoReplayError) Unwrap() error  { return e.err }
func (e *recoveryNoReplayError) NoReplay() bool { return true }

type recoveryNoReplayModel struct {
	fantasy.LanguageModel
}

func (m *recoveryNoReplayModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		stream(func(part fantasy.StreamPart) bool {
			if part.Type == fantasy.StreamPartTypeError && part.Error != nil {
				part.Error = &recoveryNoReplayError{err: part.Error}
			}
			return yield(part)
		})
	}, nil
}

func (rt recoveryFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	forwarded := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = rt.target.Scheme, rt.target.Host
	forwarded.URL, forwarded.Host = &u, rt.target.Host
	return rt.base.RoundTrip(forwarded)
}

type recoveryFailureTrace struct {
	mu               sync.Mutex
	stages           []string
	models           []string
	statuses         []int
	bodyBytes        []int
	bodyHashes       []string
	codexCount       atomic.Int32
	workerCount      atomic.Int32
	transportHijack  atomic.Bool
	workerModel      string
	summaryPointer   bool
	pinnedHash       string
	sessionHash      string
	globalConfigHash string
	globalDataHash   string
	localProxyNone   bool
	codexAuthOK      bool
	workerAuthOK     bool
	codexHostOK      bool
	workerHostOK     bool
}

func (d *recoveryFailureTrace) record(stage, model string, body []byte, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.stages) >= 6 {
		return
	}
	h := sha256.Sum256(body)
	d.stages = append(d.stages, stage)
	d.models = append(d.models, model)
	d.statuses = append(d.statuses, status)
	d.bodyBytes = append(d.bodyBytes, len(body))
	d.bodyHashes = append(d.bodyHashes, hex.EncodeToString(h[:6]))
}

func (d *recoveryFailureTrace) failureReport(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Errorf("redacted recovery failure trace: stages=%v models=%v statuses=%v request_body_bytes=%v body_hashes=%v codex_requests=%d worker_requests=%d worker_model=%s summary_pointer=%t session_hash=%s pinned_hash=%s global_config_hash=%s global_data_hash=%s local_proxy_none=%t codex_auth_match=%t worker_auth_match=%t codex_host_match=%t worker_host_match=%t",
			d.stages, d.models, d.statuses, d.bodyBytes, d.bodyHashes, d.codexCount.Load(), d.workerCount.Load(), d.workerModel, d.summaryPointer, d.sessionHash, d.pinnedHash, d.globalConfigHash, d.globalDataHash, d.localProxyNone, d.codexAuthOK, d.workerAuthOK, d.codexHostOK, d.workerHostOK)
	})
}

func recoveryFailureHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:5])
}

func recoveryFailureRequestModel(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	return request.Model
}

type recoveryFailureHarness struct {
	f             *attemptFixture
	pinned        *resolvedOverrides
	trace         *recoveryFailureTrace
	workerHost    string
	codexHost     string
	workerStarted chan struct{}
	workerMode    string
	codexMode     string
}

func newRecoveryFailureHarness(t *testing.T, workerMode, codexMode string) *recoveryFailureHarness {
	t.Helper()
	globalConfig, globalData := isolateAllGlobalConfigPaths(t)
	d := &recoveryFailureTrace{
		codexAuthOK: true, workerAuthOK: true, codexHostOK: true, workerHostOK: true,
		localProxyNone: true, globalConfigHash: recoveryFailureHash(globalConfig), globalDataHash: recoveryFailureHash(globalData),
	}
	d.failureReport(t)
	h := &recoveryFailureHarness{trace: d, workerStarted: make(chan struct{}, 1), workerMode: workerMode, codexMode: codexMode}

	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		h.trace.workerCount.Add(1)
		var request struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &request)
		h.trace.mu.Lock()
		h.trace.workerModel = request.Model
		h.trace.workerAuthOK = h.trace.workerAuthOK && r.Header.Get("Authorization") == "Bearer failure-worker-token"
		h.trace.workerHostOK = h.trace.workerHostOK && strings.EqualFold(r.Host, h.workerHost)
		h.trace.mu.Unlock()
		switch h.workerMode {
		case "http-400", "http-400-context-overflow":
			h.trace.record("worker-http-400", request.Model, body, http.StatusBadRequest)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			if h.workerMode == "http-400-context-overflow" {
				_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"worker summary exceeds context"}}`)
			} else {
				_, _ = io.WriteString(w, `{"error":{"message":"worker rejected summary"}}`)
			}
			select {
			case h.workerStarted <- struct{}{}:
			default:
			}
		case "cancel-stream":
			h.trace.record("worker-cancel-stream", request.Model, body, http.StatusOK)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"partial\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"incomplete summary\"},\"finish_reason\":null}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case h.workerStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		default:
			h.trace.record("worker-summary", request.Model, body, http.StatusOK)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Persisted failure regression summary.\"},\"finish_reason\":null}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":12,\"total_tokens\":92}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(workerServer.Close)
	h.workerHost = recoveryFailureHost(workerServer.URL)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(h.trace.codexCount.Add(1))
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		h.trace.mu.Lock()
		h.trace.codexAuthOK = h.trace.codexAuthOK && r.Header.Get("Authorization") == "Bearer failure-codex-token"
		h.trace.codexHostOK = h.trace.codexHostOK && strings.EqualFold(r.Host, h.codexHost)
		h.trace.mu.Unlock()
		stage := "codex-http-400"
		status := http.StatusBadRequest
		switch h.codexMode {
		case "opaque-http":
			stage = "codex-opaque-http"
		case "partial-stream":
			stage, status = "codex-partial-stream", http.StatusOK
		case "transport-failure":
			if call == 1 {
				stage, status = "codex-transport-failure", 0
			} else {
				stage, status = "codex-terminal-http-400", http.StatusBadRequest
			}
		case "safe-sse-error-nested", "safe-sse-error-flat", "no-replay-safe-sse-error-nested", "no-replay-safe-sse-error-flat":
			stage, status = "codex-safe-sse-rejection", http.StatusOK
		}
		if call > 2 && h.codexMode == "transport-failure" {
			stage, status = "codex-request-ceiling", http.StatusPaymentRequired
		}
		h.trace.record(stage, recoveryFailureRequestModel(body), body, status)
		if call > 2 && h.codexMode == "transport-failure" {
			http.Error(w, "bounded fixture request ceiling", http.StatusPaymentRequired)
			return
		}
		if call > 5 {
			http.Error(w, "bounded fixture request ceiling", http.StatusPaymentRequired)
			return
		}
		switch h.codexMode {
		case "opaque-http":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"request exceeds context"},"output":[{"type":"message"}]}`)
		case "partial-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial output\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"request exceeds context\"}}}\n\n")
		case "transport-failure":
			if call == 1 {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "fixture cannot hijack transport", http.StatusBadGateway)
					return
				}
				conn, _, err := hijacker.Hijack()
				if err != nil {
					http.Error(w, "fixture cannot hijack transport", http.StatusBadGateway)
					return
				}
				_ = conn.Close()
				h.trace.transportHijack.Store(true)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_prompt","message":"fixture terminal transport control"}}`)
		case "safe-sse-error-nested", "no-replay-safe-sse-error-nested":
			overflowRecoverySSERejection(w, "error-nested")
		case "safe-sse-error-flat", "no-replay-safe-sse-error-flat":
			overflowRecoverySSERejection(w, "error-flat")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, recoveryFailureCodexBody)
		}
	}))
	t.Cleanup(codexServer.Close)
	h.codexHost = recoveryFailureHost(codexServer.URL)

	f := newAttemptFixture(t, "codex-recovery-failure", attemptFixtureOpts{noIdle: true})
	target, err := url.Parse(codexServer.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	provider, err := codexprovider.New(&http.Client{Transport: recoveryFailureTransport{target: target, base: transport}, Timeout: 3 * time.Second}, &oauth.Token{AccessToken: "failure-codex-token"})
	require.NoError(t, err)
	codexModel, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	if codexMode == "no-replay-safe-http" || codexMode == "no-replay-safe-sse-error-nested" || codexMode == "no-replay-safe-sse-error-flat" {
		codexModel = &recoveryNoReplayModel{LanguageModel: codexModel}
	}
	d.localProxyNone = transport.Proxy == nil

	workerType := catwalk.TypeOpenAICompat
	if workerMode == "cli-worker" {
		workerType = catwalk.Type(cliprovider.ProviderType)
	}
	workerConfig := config.ProviderConfig{
		ID: "failure-worker", Type: workerType, BaseURL: workerServer.URL + "/v1", APIKey: "failure-worker-token",
		Network: &config.NetworkConfig{Proxy: config.ProxyNone},
		Models:  []catwalk.Model{{ID: "failure-worker-model", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	}
	d.localProxyNone = d.localProxyNone && workerConfig.Network.Proxy == config.ProxyNone
	codexConfig := config.ProviderConfig{
		ID: codexprovider.Name, Type: "openai-codex", OAuthToken: &oauth.Token{AccessToken: "failure-codex-token"},
		Models: []catwalk.Model{{ID: "gpt-5-codex", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	}
	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir()},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Models:    make(map[config.SelectedModelType]config.SelectedModel),
	}, config.IdentityResolver())
	store.Config().Providers.Set(codexConfig.ID, codexConfig)
	store.Config().Providers.Set(workerConfig.ID, workerConfig)
	store.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: codexConfig.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: codexConfig.ID, Model: "gpt-5-codex"}
	if workerMode != "missing-worker" {
		store.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: workerConfig.ID, Model: "failure-worker-model"}
	}
	store.Config().SetupAgents()
	f.coord.cfg, f.sa.config = store, store
	f.coord.sessions, f.coord.messages, f.coord.permissions = f.env.sessions, f.env.messages, f.env.permissions
	f.coord.currentAgent = f.sa
	f.coord.skillTracker = skills.NewTracker(nil)
	model := Model{Model: codexModel, CatwalkCfg: codexConfig.Models[0], ModelCfg: store.Config().Models[config.SelectedModelTypeSmart]}
	f.model = model
	f.sa.SetModels(model, model)
	f.sa.disableAutoSummarize = false
	h.f = f
	h.pinned = &resolvedOverrides{smart: model, fast: model, providerCfg: codexConfig}
	d.sessionHash = recoveryFailureHash(f.sessID)

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	var pinnedID string
	for i := 0; i < 30; i++ {
		text := fmt.Sprintf("eligible older history %02d %s", i, strings.Repeat("useful context ", 85))
		row, createErr := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}})
		require.NoError(t, createErr)
		if i == 2 {
			pinnedID = row.ID
		}
		_, createErr = f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "historical answer " + strings.Repeat("answer context ", 70)}}})
		require.NoError(t, createErr)
	}
	require.NotEmpty(t, pinnedID)
	require.NoError(t, f.env.messages.SetPinned(ctx, pinnedID, true))
	rows, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	for _, row := range rows {
		if strings.Contains(row.FullText(), "pinned authority: preserve this scope") {
			d.pinnedHash = recoveryFailureHash(row.ID)
		}
	}
	return h
}

func recoveryFailureHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

func (h *recoveryFailureHarness) execute(t *testing.T, prompt string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := h.f.coord.runInternal(ctx, h.f.sessID, prompt, h.pinned)
	return err
}

func recordRecoverySummaryPointer(t *testing.T, h *recoveryFailureHarness) {
	t.Helper()
	sess, err := h.f.env.sessions.Get(t.Context(), h.f.sessID)
	require.NoError(t, err)
	h.trace.mu.Lock()
	h.trace.summaryPointer = sess.SummaryMessageID != ""
	h.trace.mu.Unlock()
}

func recoveryFailureSnapshot(t *testing.T, f *attemptFixture) map[string]message.Message {
	t.Helper()
	rows, err := f.env.messages.List(t.Context(), f.sessID)
	require.NoError(t, err)
	out := make(map[string]message.Message, len(rows))
	for _, row := range rows {
		if strings.HasPrefix(row.FullText(), "eligible older history") || strings.Contains(row.FullText(), "historical answer") {
			out[row.ID] = row
		}
	}
	return out
}

func requireRecoveryHistoryPreserved(t *testing.T, before, after map[string]message.Message) {
	t.Helper()
	require.Len(t, after, len(before), "recovery failure must not delete eligible original history")
	for id, row := range before {
		got, ok := after[id]
		require.True(t, ok, "original message %s survives", recoveryFailureHash(id))
		require.Equal(t, row.FullText(), got.FullText())
		require.Equal(t, row.Pinned, got.Pinned)
	}
}

func requireNoRecoverySummary(t *testing.T, f *attemptFixture) {
	t.Helper()
	sess, err := f.env.sessions.Get(t.Context(), f.sessID)
	require.NoError(t, err)
	require.Empty(t, sess.SummaryMessageID)
	rows, err := f.env.messages.List(t.Context(), f.sessID)
	require.NoError(t, err)
	for _, row := range rows {
		require.False(t, row.IsSummaryMessage, "failed recovery summary must be cleaned up")
	}
}

func TestCodexRecoveryFailureWorkerUnavailableIsTerminal(t *testing.T) {
	for _, workerMode := range []string{"missing-worker", "cli-worker"} {
		t.Run(workerMode, func(t *testing.T) {
			h := newRecoveryFailureHarness(t, workerMode, "safe-http-400")
			before := recoveryFailureSnapshot(t, h.f)
			err := h.execute(t, "Report the scheduled work briefly.")
			require.Error(t, err)
			recordRecoverySummaryPointer(t, h)
			require.EqualValues(t, 1, h.trace.codexCount.Load())
			require.Zero(t, h.trace.workerCount.Load())
			requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
			requireNoRecoverySummary(t, h.f)
			require.True(t, h.trace.localProxyNone)
			require.True(t, h.trace.codexAuthOK)
			require.True(t, h.trace.codexHostOK)
		})
	}
}

func TestCodexRecoveryWorkerHTTP400CleansSummaryAndPreservesHistory(t *testing.T) {
	for _, workerMode := range []string{"http-400", "http-400-context-overflow"} {
		t.Run(workerMode, func(t *testing.T) {
			h := newRecoveryFailureHarness(t, workerMode, "safe-http-400")
			before := recoveryFailureSnapshot(t, h.f)
			err := h.execute(t, "Report the scheduled work briefly.")
			require.Error(t, err)
			recordRecoverySummaryPointer(t, h)
			require.EqualValues(t, 1, h.trace.codexCount.Load())
			require.EqualValues(t, 1, h.trace.workerCount.Load())
			require.Equal(t, "failure-worker-model", h.trace.workerModel)
			requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
			requireNoRecoverySummary(t, h.f)
			require.True(t, h.trace.workerAuthOK)
			require.True(t, h.trace.workerHostOK)
			require.True(t, h.trace.codexAuthOK)
			require.True(t, h.trace.codexHostOK)
		})
	}
}

func TestCodexRecoveryCancellationDuringWorkerStreamCleansSummary(t *testing.T) {
	h := newRecoveryFailureHarness(t, "cancel-stream", "safe-http-400")
	before := recoveryFailureSnapshot(t, h.f)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.f.coord.runInternal(ctx, h.f.sessID, "Report the scheduled work briefly.", h.pinned)
		done <- err
	}()
	select {
	case <-h.workerStarted:
	case <-time.After(4 * time.Second):
		cancel()
		t.Fatal("worker stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(4 * time.Second):
		t.Fatal("canceled recovery did not stop")
	}
	require.EqualValues(t, 1, h.trace.codexCount.Load())
	require.EqualValues(t, 1, h.trace.workerCount.Load())
	recordRecoverySummaryPointer(t, h)
	requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
	requireNoRecoverySummary(t, h.f)
}

func TestCodexRecoverySecondOverflowStopsAfterOneSummary(t *testing.T) {
	for _, codexMode := range []string{"safe-http-400", "safe-sse-error-nested", "safe-sse-error-flat"} {
		t.Run(codexMode, func(t *testing.T) {
			h := newRecoveryFailureHarness(t, "success", codexMode)
			tool := &overflowRecoveryTool{text: "must never execute"}
			h.f.sa.SetTools([]fantasy.AgentTool{tool})
			err := h.execute(t, "Report the scheduled work briefly.")
			require.Error(t, err)
			recordRecoverySummaryPointer(t, h)
			require.EqualValues(t, 2, h.trace.codexCount.Load(), "one initial rejection and one continuation rejection")
			require.EqualValues(t, 1, h.trace.workerCount.Load(), "continuation failure must not schedule another summary")
			var codexModels []string
			h.trace.mu.Lock()
			for i, stage := range h.trace.stages {
				if strings.HasPrefix(stage, "codex-") {
					codexModels = append(codexModels, h.trace.models[i])
				}
			}
			h.trace.mu.Unlock()
			require.Equal(t, []string{"gpt-5-codex", "gpt-5-codex"}, codexModels)
			require.Zero(t, tool.calls.Load(), "no rejected request can replay a tool")
			sess, err := h.f.env.sessions.Get(t.Context(), h.f.sessID)
			require.NoError(t, err)
			require.NotEmpty(t, sess.SummaryMessageID)
			rows, err := h.f.env.messages.List(t.Context(), h.f.sessID)
			require.NoError(t, err)
			summaries := 0
			for _, row := range rows {
				if row.IsSummaryMessage {
					summaries++
				}
			}
			require.Equal(t, 1, summaries)
			require.True(t, h.trace.codexAuthOK)
			require.True(t, h.trace.codexHostOK)
			require.True(t, h.trace.workerAuthOK)
			require.True(t, h.trace.workerHostOK)
		})
	}
}

func TestCodexRecoveryNoReplayMarkerStopsOtherwiseSafeHTTPRecovery(t *testing.T) {
	for _, codexMode := range []string{"no-replay-safe-http", "no-replay-safe-sse-error-nested", "no-replay-safe-sse-error-flat"} {
		t.Run(codexMode, func(t *testing.T) {
			h := newRecoveryFailureHarness(t, "success", codexMode)
			before := recoveryFailureSnapshot(t, h.f)
			err := h.execute(t, "Report the scheduled work briefly.")
			require.Error(t, err)
			recordRecoverySummaryPointer(t, h)
			require.EqualValues(t, 1, h.trace.codexCount.Load())
			require.Zero(t, h.trace.workerCount.Load(), "NoReplay wrapped rejection must not launch worker")
			require.Empty(t, h.trace.workerModel, "configured worker must exist so this check is non-vacuous")
			requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
			requireNoRecoverySummary(t, h.f)
		})
	}
}

func TestCodexRecoveryOpaqueOrPartialFailureNeverStartsWorker(t *testing.T) {
	for _, codexMode := range []string{"opaque-http", "partial-stream"} {
		t.Run(codexMode, func(t *testing.T) {
			h := newRecoveryFailureHarness(t, "success", codexMode)
			before := recoveryFailureSnapshot(t, h.f)
			err := h.execute(t, "Report the scheduled work briefly.")
			require.Error(t, err)
			recordRecoverySummaryPointer(t, h)
			require.EqualValues(t, 1, h.trace.codexCount.Load())
			require.Zero(t, h.trace.workerCount.Load(), "ambiguous failures must not be treated as safe rejections")
			requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
			requireNoRecoverySummary(t, h.f)
		})
	}
}

func TestCodexRecoveryTransportRetryNeverStartsWorker(t *testing.T) {
	h := newRecoveryFailureHarness(t, "success", "transport-failure")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	const seededCallID = "call_transport_seed"
	seededAssistant, err := h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: seededCallID, Name: "overflow_once", Input: `{}`, Finished: true},
		},
	})
	require.NoError(t, err)
	seededAssistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, h.f.env.messages.Update(ctx, seededAssistant))
	seededResult, err := h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: seededCallID, Name: "overflow_once", Content: "seeded tool result"},
		},
	})
	require.NoError(t, err)
	tool := &overflowRecoveryTool{text: "must never execute"}
	h.f.sa.SetTools([]fantasy.AgentTool{tool})

	before := recoveryFailureSnapshot(t, h.f)
	beforeRows, err := h.f.env.messages.List(ctx, h.f.sessID)
	require.NoError(t, err)
	beforeMessages := make(map[string]message.Message, len(beforeRows))
	for _, row := range beforeRows {
		beforeMessages[row.ID] = row
	}
	beforeSeeded := map[string]string{seededAssistant.ID: seededAssistant.FullText(), seededResult.ID: seededResult.FullText()}
	err = h.execute(t, "Report the scheduled work briefly.")
	require.Error(t, err)
	recordRecoverySummaryPointer(t, h)
	require.EqualValues(t, 2, h.trace.codexCount.Load(), "the terminal fixture response must bound the retry")
	require.True(t, h.trace.transportHijack.Load(), "first request must fail by hijacking and closing the connection")
	require.Zero(t, h.trace.workerCount.Load(), "a transport failure is not a certified context rejection")
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, http.StatusBadRequest, providerErr.StatusCode)
	require.False(t, providerErr.IsContextTooLarge())
	require.Contains(t, providerErr.Message, "fixture terminal transport control")

	h.trace.mu.Lock()
	stages := append([]string(nil), h.trace.stages...)
	statuses := append([]int(nil), h.trace.statuses...)
	models := append([]string(nil), h.trace.models...)
	bodyBytes := append([]int(nil), h.trace.bodyBytes...)
	bodyHashes := append([]string(nil), h.trace.bodyHashes...)
	h.trace.mu.Unlock()
	require.Equal(t, []string{"codex-transport-failure", "codex-terminal-http-400"}, stages)
	require.Equal(t, []int{0, http.StatusBadRequest}, statuses)
	require.Equal(t, []string{"gpt-5-codex", "gpt-5-codex"}, models)
	require.Equal(t, bodyBytes[0], bodyBytes[1])
	require.Equal(t, bodyHashes[0], bodyHashes[1])
	requireRecoveryHistoryPreserved(t, before, recoveryFailureSnapshot(t, h.f))
	requireNoRecoverySummary(t, h.f)

	rows, err := h.f.env.messages.List(t.Context(), h.f.sessID)
	require.NoError(t, err)
	afterMessages := make(map[string]message.Message, len(rows))
	for _, row := range rows {
		afterMessages[row.ID] = row
	}
	for id, original := range beforeMessages {
		got, ok := afterMessages[id]
		require.True(t, ok, "original message %s remains", recoveryFailureHash(id))
		require.Equal(t, original.FullText(), got.FullText())
		require.Equal(t, original.Pinned, got.Pinned)
	}
	seededFound := make(map[string]string, len(beforeSeeded))
	toolCalls, toolResults := 0, 0
	for _, row := range rows {
		for _, call := range row.ToolCalls() {
			toolCalls++
			require.Equal(t, seededCallID, call.ID)
		}
		for _, result := range row.ToolResults() {
			toolResults++
			require.Equal(t, seededCallID, result.ToolCallID)
		}
		if row.ID == seededAssistant.ID {
			seededFound[row.ID] = row.FullText()
			require.Equal(t, seededAssistant.ToolCalls(), row.ToolCalls(), "seeded accepted tool call remains unchanged")
		}
		if row.ID == seededResult.ID {
			seededFound[row.ID] = row.FullText()
			require.Equal(t, seededResult.ToolResults(), row.ToolResults(), "seeded tool result remains unchanged")
		}
	}
	require.Equal(t, 1, toolCalls, "failed requests must not add a tool call")
	require.Equal(t, 1, toolResults, "failed requests must not add a tool result")
	require.Equal(t, beforeSeeded, seededFound, "seeded completed tool exchange remains unchanged")
	require.Zero(t, tool.calls.Load(), "the configured tool must not execute")
}

func TestCodexRecoverySafeHTTPRejectionFollowsOverlargeOriginalRequest(t *testing.T) {
	var requestBytes atomic.Int64
	f, pinned, diagnostics := newCodexOverflowRun(t, "codex-recovery-overlarge-failure", nil, func(int) http.HandlerFunc {
		return func(w http.ResponseWriter, request *http.Request) {
			requestBytes.Store(request.ContentLength)
			if request.ContentLength <= 24<<10 {
				http.Error(w, "fixture request did not exceed the configured bound", http.StatusPaymentRequired)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, recoveryFailureCodexBody)
		}
	})
	f.sa.disableAutoSummarize = false
	f.coord.cfg.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	_, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "eligible oversized history " + strings.Repeat("x", 24800)}},
	})
	require.NoError(t, err)
	before, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	_, err = f.coord.runInternal(ctx, f.sessID, "Report the scheduled work briefly.", pinned)
	require.Error(t, err)
	require.Greater(t, requestBytes.Load(), int64(24<<10), "provider must send an actually overlarge serialized request")
	require.EqualValues(t, 1, diagnostics.requestCount(), "one exact structured Codex HTTP 400; requests=%d", diagnostics.requestCount())
	require.Equal(t, "gpt-5-codex", diagnostics.observedModelID())
	require.EqualValues(t, 1, diagnostics.pinCount())
	sessionAfter, err := f.env.sessions.Get(ctx, f.sessID)
	require.NoError(t, err)
	require.Empty(t, sessionAfter.SummaryMessageID)
	after, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	require.Len(t, after, len(before)+2, "only the original user prompt and failed assistant turn may be added")
	for _, row := range before {
		var found bool
		for _, got := range after {
			if got.ID == row.ID {
				found = true
				require.Equal(t, row.FullText(), got.FullText())
				require.Equal(t, row.Pinned, got.Pinned)
				break
			}
		}
		require.True(t, found, "original history row %s remains", recoveryFailureHash(row.ID))
	}
	require.False(t, f.sa.IsSessionBusy(f.sessID), "failed automatic recovery releases session ownership")
}
