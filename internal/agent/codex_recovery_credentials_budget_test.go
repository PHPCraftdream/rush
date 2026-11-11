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
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/stretchr/testify/require"
)

const credentialRecoveryOverflow = `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"request exceeds context"}}`

const credentialRecoveryMaxBody = 38 << 10
const credentialRecoveryMaxRequests = 6

type credentialRecoveryFacts struct {
	mu                                        sync.Mutex
	codexCalls, tenantCalls, operatorCalls    atomic.Int32
	model, workerModel                        string
	codexModels                               []string
	codexStatuses                             []int
	codexBodyBytes                            []int
	codexHasTool, codexHasAuthority           []bool
	codexAuthOK, tenantAuthOK, operatorAuthOK bool
	overLimit                                 bool
}

func (d *credentialRecoveryFacts) install(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Errorf("redacted recovery trace: codex=%d tenant_worker=%d operator_worker=%d model=%s worker_model=%s statuses=%v body_bytes=%v tools=%v authority=%v over_limit=%t auth_matches=%t/%t/%t",
			d.codexCalls.Load(), d.tenantCalls.Load(), d.operatorCalls.Load(), d.model, d.workerModel,
			d.codexStatuses, d.codexBodyBytes, d.codexHasTool, d.codexHasAuthority, d.overLimit,
			d.codexAuthOK, d.tenantAuthOK, d.operatorAuthOK)
	})
}

type credentialRecoveryHarness struct {
	f         *attemptFixture
	model     Model
	tool      *overflowRecoveryTool
	facts     *credentialRecoveryFacts
	tenantURL string
}

func newCredentialRecoveryHarness(t *testing.T, acceptedTool bool) *credentialRecoveryHarness {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	h := &credentialRecoveryHarness{
		facts: &credentialRecoveryFacts{codexAuthOK: true, tenantAuthOK: true, operatorAuthOK: true},
		tool:  &overflowRecoveryTool{text: "unused"},
	}
	h.facts.install(t)
	if acceptedTool {
		h.tool.text = strings.Repeat("tool-result-", 500)
	}

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(h.facts.codexCalls.Add(1))
		if call > credentialRecoveryMaxRequests {
			http.Error(w, "bounded credential recovery request ceiling", http.StatusPaymentRequired)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var request struct {
			Model string `json:"model"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &request)
		hasTool, hasAuthority := false, false
		for _, tool := range request.Tools {
			hasTool = hasTool || tool.Name == "overflow_once"
		}
		for _, item := range request.Input {
			for _, part := range item.Content {
				hasAuthority = hasAuthority || strings.Contains(part.Text, "pinned authority: preserve this scope")
			}
		}
		large := len(body) > credentialRecoveryMaxBody
		h.facts.mu.Lock()
		h.facts.model = request.Model
		if len(h.facts.codexModels) < credentialRecoveryMaxRequests {
			h.facts.codexModels = append(h.facts.codexModels, request.Model)
			h.facts.codexHasTool = append(h.facts.codexHasTool, hasTool)
			h.facts.codexHasAuthority = append(h.facts.codexHasAuthority, hasAuthority)
			h.facts.codexBodyBytes = append(h.facts.codexBodyBytes, len(body))
		}
		h.facts.codexAuthOK = h.facts.codexAuthOK && r.Header.Get("Authorization") == "Bearer recovery-codex-token"
		h.facts.overLimit = h.facts.overLimit || large
		h.facts.mu.Unlock()

		if acceptedTool && call == 1 {
			h.recordCodexStatus(http.StatusOK)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"id\":\"fc_recovery\",\"type\":\"function_call\",\"call_id\":\"call_recovery_once\",\"name\":\"overflow_once\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":20,\"output_tokens\":8,\"total_tokens\":28}}}\n\n")
			return
		}
		reject := (!acceptedTool && call == 1) || (acceptedTool && call == 2)
		if reject {
			if !large {
				h.recordCodexStatus(http.StatusPaymentRequired)
				http.Error(w, "bounded fixture request was below overflow threshold", http.StatusPaymentRequired)
				return
			}
			h.recordCodexStatus(http.StatusBadRequest)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, credentialRecoveryOverflow)
			return
		}
		h.recordCodexStatus(http.StatusOK)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"continued\"}]}],\"usage\":{\"input_tokens\":20,\"output_tokens\":8,\"total_tokens\":28}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(codexServer.Close)
	target, err := url.Parse(codexServer.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	codex, err := codexprovider.New(&http.Client{
		Transport: recoveryFailureTransport{target: target, base: transport}, Timeout: 3 * time.Second,
	}, &oauth.Token{AccessToken: "recovery-codex-token"})
	require.NoError(t, err)
	codexModel, err := codex.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	tenantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.facts.tenantCalls.Add(1) > credentialRecoveryMaxRequests {
			http.Error(w, "bounded tenant worker request ceiling", http.StatusPaymentRequired)
			return
		}
		h.recordWorker(r, "tenant-fixture-key")
		credentialRecoverySummary(w)
	}))
	t.Cleanup(tenantServer.Close)
	h.tenantURL = tenantServer.URL + "/v1"
	operatorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.facts.operatorCalls.Add(1) > credentialRecoveryMaxRequests {
			http.Error(w, "bounded operator worker request ceiling", http.StatusPaymentRequired)
			return
		}
		h.recordWorker(r, "operator-fixture-key")
		credentialRecoverySummary(w)
	}))
	t.Cleanup(operatorServer.Close)

	h.f = newAttemptFixture(t, "credentialed-codex-recovery", attemptFixtureOpts{noIdle: true})
	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir(), Network: &config.NetworkConfig{Proxy: config.ProxyNone}},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Models:    make(map[config.SelectedModelType]config.SelectedModel),
	}, config.IdentityResolver())
	codexCfg := config.ProviderConfig{
		ID: codexprovider.Name, Type: catwalk.Type("openai-codex"), OAuthToken: &oauth.Token{AccessToken: "recovery-codex-token"},
		Models: []catwalk.Model{{ID: "gpt-5-codex", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	}
	operatorCfg := config.ProviderConfig{
		ID: "operator-worker", Type: catwalk.TypeOpenAICompat, BaseURL: operatorServer.URL + "/v1", APIKey: "operator-fixture-key",
		Network: &config.NetworkConfig{Proxy: config.ProxyNone},
		Models: []catwalk.Model{{ID: "operator-worker-model", ContextWindow: 200000, DefaultMaxTokens: 1000,
			CostPer1MIn: 1000, CostPer1MOut: 1000}},
	}
	store.Config().Providers.Set(codexCfg.ID, codexCfg)
	store.Config().Providers.Set(operatorCfg.ID, operatorCfg)
	store.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: operatorCfg.ID, Model: "operator-worker-model"}
	store.Config().SetupAgents()
	h.f.coord.cfg, h.f.sa.config = store, store
	h.f.coord.sessions, h.f.coord.messages, h.f.coord.permissions = h.f.env.sessions, h.f.env.messages, h.f.env.permissions
	h.f.coord.currentAgent = h.f.sa
	h.f.coord.skillTracker = skills.NewTracker(nil)
	h.model = Model{Model: codexModel, CatwalkCfg: codexCfg.Models[0], ModelCfg: store.Config().Models[config.SelectedModelTypeSmart]}
	h.f.sa.SetModels(h.model, h.model)
	h.f.sa.SetTools([]fantasy.AgentTool{h.tool})
	h.f.sa.disableAutoSummarize = false
	h.seedHistory(t)
	return h
}

func (h *credentialRecoveryHarness) recordCodexStatus(status int) {
	h.facts.mu.Lock()
	if len(h.facts.codexStatuses) < credentialRecoveryMaxRequests {
		h.facts.codexStatuses = append(h.facts.codexStatuses, status)
	}
	h.facts.mu.Unlock()
}

func (h *credentialRecoveryHarness) recordWorker(r *http.Request, token string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	h.facts.mu.Lock()
	if token == "tenant-fixture-key" {
		h.facts.tenantAuthOK = h.facts.tenantAuthOK && r.Header.Get("Authorization") == "Bearer "+token
	} else {
		h.facts.operatorAuthOK = h.facts.operatorAuthOK && r.Header.Get("Authorization") == "Bearer "+token
	}
	h.facts.workerModel = request.Model
	h.facts.mu.Unlock()
}

func (h *credentialRecoveryHarness) seedHistory(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	_, err := h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "pinned authority: preserve this scope"}},
	})
	require.NoError(t, err)
	rows, err := h.f.env.messages.List(ctx, h.f.sessID)
	require.NoError(t, err)
	var authorityID string
	for _, row := range rows {
		if strings.Contains(row.FullText(), "pinned authority: preserve this scope") {
			authorityID = row.ID
		}
	}
	require.NotEmpty(t, authorityID)
	require.NoError(t, h.f.env.messages.SetPinned(ctx, authorityID, true))
	for i := 0; i < 42; i++ {
		_, err = h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{
			message.TextContent{Text: "eligible older history " + strings.Repeat("u", 700)},
		}})
		require.NoError(t, err)
		_, err = h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{
			message.TextContent{Text: "historical answer " + strings.Repeat("a", 400)},
		}})
		require.NoError(t, err)
	}
}

func credentialRecoverySummary(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Persisted credential recovery summary.\"},\"finish_reason\":null}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":12,\"total_tokens\":92}}\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (h *credentialRecoveryHarness) call(t *testing.T, creds *CredentialSet, maxTokens int64, maxCost float64) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	_, err := h.f.sa.Run(ctx, SessionAgentCall{
		SessionID: h.f.sessID, Prompt: "Continue the original request.", SmartModel: &h.model, FastModel: &h.model,
		Tools: []fantasy.AgentTool{h.tool}, Credentials: creds, MaxTokens: maxTokens, MaxCost: maxCost,
	})
	return err
}

func credentialRecoverySet(worker, allowFallback bool, baseURL string) *CredentialSet {
	models := map[Role]ModelChoice{RoleSmart: {Provider: "tenant-recovery", Model: "tenant-smart-model"}}
	if worker {
		models[RoleWorker] = ModelChoice{Provider: "tenant-recovery", Model: "tenant-worker-model"}
	}
	return &CredentialSet{
		Credentials: []Credential{{Provider: "tenant-recovery", Type: ProviderTypeOpenAICompat, APIKey: "tenant-fixture-key", BaseURL: baseURL,
			Models: []CredentialModel{{ID: "tenant-smart-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
				{ID: "tenant-worker-model", ContextWindow: 200000, DefaultMaxTokens: 1000}}}},
		Models: models, AllowConfiguredRoleFallback: allowFallback,
	}
}

func credentialRecoveryHistory(t *testing.T, h *credentialRecoveryHarness) map[string]message.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.f.env.messages.List(ctx, h.f.sessID)
	require.NoError(t, err)
	out := make(map[string]message.Message)
	for _, row := range rows {
		if strings.HasPrefix(row.FullText(), "eligible older history") || strings.HasPrefix(row.FullText(), "historical answer") || strings.Contains(row.FullText(), "pinned authority: preserve this scope") {
			out[row.ID] = row
		}
	}
	return out
}

func requireCredentialRecoveryHistory(t *testing.T, h *credentialRecoveryHarness, before map[string]message.Message) {
	t.Helper()
	after := credentialRecoveryHistory(t, h)
	require.Len(t, after, len(before), "recovery refusal leaves original history unchanged")
	for id, row := range before {
		got, ok := after[id]
		require.True(t, ok, "original history row %s remains", overflowRecoveryHash(id))
		require.Equal(t, row.FullText(), got.FullText())
		require.Equal(t, row.Pinned, got.Pinned)
	}
}

func TestCodexRecoveryPerCallWorkerCredentialRetainsOriginalAuthority(t *testing.T) {
	h := newCredentialRecoveryHarness(t, false)
	err := h.call(t, credentialRecoverySet(true, true, h.tenantURL), 0, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, h.facts.codexCalls.Load(), "initial overflow plus original-model continuation")
	require.EqualValues(t, 1, h.facts.tenantCalls.Load())
	require.Zero(t, h.facts.operatorCalls.Load(), "covered tenant worker never invokes configured operator worker")
	require.Equal(t, "tenant-worker-model", h.facts.workerModel)
	require.True(t, h.facts.codexAuthOK)
	require.True(t, h.facts.tenantAuthOK)
	h.facts.mu.Lock()
	models := append([]string(nil), h.facts.codexModels...)
	statuses := append([]int(nil), h.facts.codexStatuses...)
	tools := append([]bool(nil), h.facts.codexHasTool...)
	authority := append([]bool(nil), h.facts.codexHasAuthority...)
	overLimit := h.facts.overLimit
	h.facts.mu.Unlock()
	require.Equal(t, []string{"gpt-5-codex", "gpt-5-codex"}, models, "both calls retain the original direct Codex model")
	require.Equal(t, []int{http.StatusBadRequest, http.StatusOK}, statuses)
	require.Equal(t, []bool{true, true}, tools, "the call's original tool set remains on recovery continuation")
	require.Equal(t, []bool{true, true}, authority, "pinned authority remains on original and continuation requests")
	require.True(t, overLimit, "first Codex request is beyond the local rejection threshold")
	sess, err := h.f.env.sessions.Get(t.Context(), h.f.sessID)
	require.NoError(t, err)
	require.NotEmpty(t, sess.SummaryMessageID)
}

func TestCodexRecoveryPerCallWorkerCredentialMissingCoverageFailsClosed(t *testing.T) {
	h := newCredentialRecoveryHarness(t, false)
	before := credentialRecoveryHistory(t, h)
	err := h.call(t, credentialRecoverySet(false, false, h.tenantURL), 0, 0)
	require.Error(t, err)
	require.EqualValues(t, 1, h.facts.codexCalls.Load())
	require.Zero(t, h.facts.tenantCalls.Load())
	require.Zero(t, h.facts.operatorCalls.Load(), "uncovered worker must not reach operator without explicit fallback")
	sess, getErr := h.f.env.sessions.Get(t.Context(), h.f.sessID)
	require.NoError(t, getErr)
	require.Empty(t, sess.SummaryMessageID)
	requireCredentialRecoveryHistory(t, h, before)
}

func TestCodexRecoveryPerCallMaxTokensCountsSummaryOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxTokens int64
		continues bool
	}{
		{name: "summary crosses cap", maxTokens: 119},
		{name: "primary and summary leave room for continuation", maxTokens: 148, continues: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCredentialRecoveryHarness(t, true)
			before := credentialRecoveryHistory(t, h)
			err := h.call(t, credentialRecoverySet(true, false, h.tenantURL), tc.maxTokens, 0)
			if tc.continues {
				require.NoError(t, err, "primary 28 tokens plus summary 92 and continuation 28 fit the cap exactly")
				require.EqualValues(t, 3, h.facts.codexCalls.Load(), "continuation is permitted at the exact cumulative token cap")
				require.EqualValues(t, 1, h.tool.calls.Load())
				require.EqualValues(t, 1, h.facts.tenantCalls.Load())
				require.Zero(t, h.facts.operatorCalls.Load())
				sess, getErr := h.f.env.sessions.Get(t.Context(), h.f.sessID)
				require.NoError(t, getErr)
				require.NotEmpty(t, sess.SummaryMessageID)
				return
			}
			require.Error(t, err)
			require.EqualValues(t, 2, h.facts.codexCalls.Load(), "no Codex continuation after summary crosses token cap")
			require.EqualValues(t, 1, h.tool.calls.Load(), "accepted primary tool step is not replayed")
			require.EqualValues(t, 1, h.facts.tenantCalls.Load(), "cap refusal follows actual worker summary inference")
			require.Zero(t, h.facts.operatorCalls.Load())
			sess, getErr := h.f.env.sessions.Get(t.Context(), h.f.sessID)
			require.NoError(t, getErr)
			require.Empty(t, sess.SummaryMessageID, "summary that crosses cap is not published")
			requireCredentialRecoveryHistory(t, h, before)
		})
	}
}

func TestCodexRecoveryPerCallMaxCostAccountsRejectedSummaryOnce(t *testing.T) {
	h := newCredentialRecoveryHarness(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := h.f.env.sessions.IncrementCost(ctx, h.f.sessID, 1.95)
	require.NoError(t, err)
	before := credentialRecoveryHistory(t, h)
	err = h.call(t, credentialRecoverySet(false, true, ""), 0, 2.0)
	require.Error(t, err)
	require.EqualValues(t, 1, h.facts.codexCalls.Load())
	require.EqualValues(t, 1, h.facts.operatorCalls.Load(), "configured fallback inference occurs before cost-cap decision")
	require.Zero(t, h.facts.tenantCalls.Load())
	require.True(t, h.facts.operatorAuthOK)
	require.Equal(t, "operator-worker-model", h.facts.workerModel)
	sess, getErr := h.f.env.sessions.Get(ctx, h.f.sessID)
	require.NoError(t, getErr)
	require.Empty(t, sess.SummaryMessageID, "over-budget summary is not published")
	budget, getErr := h.f.env.sessions.SubtreeBudget(ctx, h.f.sessID)
	require.NoError(t, getErr)
	require.InDelta(t, 2.042, budget, 1e-9, "fixture summary cost of $0.092 is accounted once")
	requireCredentialRecoveryHistory(t, h, before)
}
