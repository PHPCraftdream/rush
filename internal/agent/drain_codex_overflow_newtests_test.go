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
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/stretchr/testify/require"
)

type overflowRecoveryRT struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt overflowRecoveryRT) RoundTrip(req *http.Request) (*http.Response, error) {
	forwarded := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = rt.target.Scheme, rt.target.Host
	forwarded.URL, forwarded.Host = &u, rt.target.Host
	return rt.base.RoundTrip(forwarded)
}

type overflowRecoveryBodyFacts struct {
	summary, pin, user, toolCall, toolResult bool
}

type overflowRecoveryTrace struct {
	mu                                                                 sync.Mutex
	stages, modelClasses                                               []string
	bodyBytes                                                          []int
	bodyHashes                                                         []string
	statuses                                                           []int
	codexBodies                                                        []overflowRecoveryBodyFacts
	sessionHash, pinnedHash                                            string
	pins, acceptedToolCalls                                            int
	summaryPointerSet, contextDeadline, contextCanceled, ownershipBusy bool
	fixtureAuthOnly, localHostsOnly                                    bool
	codexModelExact                                                    bool
	workerModel                                                        string
}

func overflowRecoveryHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:5])
}

func overflowRecoveryHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

func overflowRecoveryModelClass(model string) string {
	if strings.Contains(model, "codex") {
		return "original"
	}
	return "worker"
}

func (d *overflowRecoveryTrace) add(stage, modelClass string, body []byte, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.stages) >= 4 {
		return
	}
	h := sha256.Sum256(body)
	d.stages = append(d.stages, stage)
	d.modelClasses = append(d.modelClasses, modelClass)
	d.bodyBytes = append(d.bodyBytes, len(body))
	d.bodyHashes = append(d.bodyHashes, hex.EncodeToString(h[:6]))
	d.statuses = append(d.statuses, status)
}

// Inspect only bounded, decoded metadata needed for assertions. No request or
// transcript text is retained in diagnostics; hashed bodies are failure-only.
func (d *overflowRecoveryTrace) inspectCodexRequest(r *http.Request, body []byte, sessionID string) {
	var req struct {
		Model string `json:"model"`
		Input []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Output  string `json:"output"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	facts := overflowRecoveryBodyFacts{}
	for _, item := range req.Input {
		for _, part := range item.Content {
			facts.summary = facts.summary || strings.Contains(part.Text, "Persisted summary of earlier context")
			facts.pin = facts.pin || strings.Contains(part.Text, "pinned authority: preserve this scope")
			facts.user = facts.user || strings.Contains(part.Text, "Summarize the useful details and continue the current task.")
		}
		facts.toolCall = facts.toolCall || item.Type == "function_call" && item.CallID == "call_recovery_once" && item.Name == "overflow_once"
		facts.toolResult = facts.toolResult || item.Type == "function_call_output" && strings.Contains(item.Output, "tool-result-")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.modelClasses) > 0 {
		d.modelClasses[len(d.modelClasses)-1] = overflowRecoveryModelClass(req.Model)
	}
	if r.Header.Get("session_id") == sessionID {
		d.codexBodies = append(d.codexBodies, facts)
		d.codexModelExact = d.codexModelExact && req.Model == "gpt-5-codex"
	}
}

func (d *overflowRecoveryTrace) install(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		t.Errorf("redacted overflow trace: stages=%v model_classes=%v body_bytes=%v body_hashes=%v statuses=%v session=%s pinned_id=%s summary_pointer_set=%t pins=%d accepted_tool_calls=%d ownership_busy=%t context_deadline=%t context_canceled=%t local_hosts_only=%t fixture_auth_only=%t worker_model=%s",
			d.stages, d.modelClasses, d.bodyBytes, d.bodyHashes, d.statuses, d.sessionHash, d.pinnedHash, d.summaryPointerSet, d.pins, d.acceptedToolCalls, d.ownershipBusy, d.contextDeadline, d.contextCanceled, d.localHostsOnly, d.fixtureAuthOnly, d.workerModel)
	})
}

type overflowRecoveryTool struct {
	calls atomic.Int32
	text  string
}

func (t *overflowRecoveryTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "overflow_once", Parameters: map[string]any{}}
}
func (t *overflowRecoveryTool) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (t *overflowRecoveryTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *overflowRecoveryTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.calls.Add(1)
	return fantasy.NewTextResponse(t.text), nil
}

func TestCodexOverflowAutomaticRecoverySameSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		workerOverride bool
		wantWorker     string
	}{
		{name: "configured worker", wantWorker: "default-worker"},
		{name: "session worker override", workerOverride: true, wantWorker: "session-worker"},
	} {
		t.Run(tc.name, func(t *testing.T) { runCodexOverflowRecovery(t, false, tc.workerOverride, tc.wantWorker) })
	}
}

func TestCodexOverflowAutomaticRecoveryAfterAcceptedToolStep(t *testing.T) {
	runCodexOverflowRecovery(t, true, true, "session-worker")
}

func runCodexOverflowRecovery(t *testing.T, acceptedTool, workerOverride bool, wantWorker string) {
	t.Helper()
	// Reuse the isolated session/services and pinned seed. Recovery and summary
	// are performed by ordinary production paths; no summary is pre-created.
	trace := &overflowRecoveryTrace{fixtureAuthOnly: true, localHostsOnly: true, codexModelExact: true}
	trace.install(t)
	f, _, _ := newCodexOverflowRun(t, "codex-overflow-recovery", nil, func(int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { overflowRecoveryReply(w, "unused legacy handler") }
	})
	trace.sessionHash = overflowRecoveryHash(f.sessID)

	var codexCalls, workerCalls atomic.Int32
	const simpleCodexBodyLimit = 48000
	const acceptedToolCodexBodyLimit = 32000
	codexBodyLimit := simpleCodexBodyLimit
	if acceptedTool {
		codexBodyLimit = acceptedToolCodexBodyLimit
	}
	tool := &overflowRecoveryTool{text: strings.Repeat("tool-result-", 1800)}
	if acceptedTool {
		f.sa.SetTools([]fantasy.AgentTool{tool})
	}
	f.sa.disableAutoSummarize = false

	workerHost := ""
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		workerCalls.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		trace.mu.Lock()
		trace.fixtureAuthOnly = trace.fixtureAuthOnly && r.Header.Get("Authorization") == "Bearer worker-fixture-token"
		trace.localHostsOnly = trace.localHostsOnly && r.Host == workerHost
		trace.workerModel = req.Model
		trace.mu.Unlock()
		trace.add("worker-summary", "worker", body, http.StatusOK)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Persisted summary of earlier context.\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"s\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":12,\"total_tokens\":112}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(workerServer.Close)
	workerHost = overflowRecoveryHost(workerServer.URL)

	codexHost := ""
	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(codexCalls.Add(1))
		if call > 4 {
			http.Error(w, "bounded Codex test request ceiling", http.StatusPaymentRequired)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		trace.mu.Lock()
		trace.fixtureAuthOnly = trace.fixtureAuthOnly && r.Header.Get("Authorization") == "Bearer local-fixture-token"
		trace.localHostsOnly = trace.localHostsOnly && r.Host == codexHost
		trace.mu.Unlock()
		status, stage := http.StatusOK, "codex-original"
		if acceptedTool && call == 1 {
			stage = "codex-accepted-tool-step"
		} else if len(body) > codexBodyLimit {
			stage, status = "codex-context-overflow", http.StatusBadRequest
		}
		trace.add(stage, overflowRecoveryModelClass(req.Model), body, status)
		trace.inspectCodexRequest(r, body, f.sessID)
		if status == http.StatusBadRequest {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"request exceeds context"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if acceptedTool && call == 1 {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"id\":\"fc_recovery\",\"type\":\"function_call\",\"call_id\":\"call_recovery_once\",\"name\":\"overflow_once\",\"arguments\":\"{}\"}]}}\n\n")
			return
		}
		overflowRecoveryReply(w, "continued after persisted summary")
	}))
	t.Cleanup(codexServer.Close)
	codexHost = overflowRecoveryHost(codexServer.URL)
	target, err := url.Parse(codexServer.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	provider, err := codexprovider.New(&http.Client{Transport: overflowRecoveryRT{target: target, base: transport}, Timeout: 3 * time.Second}, &oauth.Token{AccessToken: "local-fixture-token"})
	require.NoError(t, err)
	codex, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir()},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Models:    make(map[config.SelectedModelType]config.SelectedModel),
	}, config.IdentityResolver())
	codexCfg := config.ProviderConfig{ID: "openai-codex", Type: "openai-compat", OAuthToken: &oauth.Token{AccessToken: "local-fixture-token"}, Models: []catwalk.Model{{ID: "gpt-5-codex", ContextWindow: 200000, DefaultMaxTokens: 1000}}}
	workerCfg := config.ProviderConfig{ID: "overflow-worker", Type: "openai-compat", BaseURL: workerServer.URL + "/v1", APIKey: "worker-fixture-token", Network: &config.NetworkConfig{Proxy: config.ProxyNone}, Models: []catwalk.Model{{ID: "default-worker", ContextWindow: 200000, DefaultMaxTokens: 1000}, {ID: "session-worker", ContextWindow: 200000, DefaultMaxTokens: 1000}}}
	store.Config().Providers.Set(codexCfg.ID, codexCfg)
	store.Config().Providers.Set(workerCfg.ID, workerCfg)
	store.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: codexCfg.ID, Model: "gpt-5-codex"}
	store.Config().Models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: workerCfg.ID, Model: "default-worker"}
	store.Config().SetupAgents()
	f.coord.cfg, f.sa.config = store, store
	f.coord.skillTracker = skills.NewTracker(nil)
	f.coord.currentAgent = f.sa
	model := Model{Model: codex, CatwalkCfg: codexCfg.Models[0], ModelCfg: store.Config().Models[config.SelectedModelTypeSmart]}
	f.sa.SetModels(model, model)
	pinned := &resolvedOverrides{smart: model, fast: model, providerCfg: codexCfg}

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	_, trace.contextDeadline = ctx.Deadline()
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
	trace.pinnedHash = overflowRecoveryHash(pinID)
	trace.pins = 1
	seedCount := 42
	if acceptedTool {
		seedCount = 8
	}
	for i := 0; i < seedCount; i++ {
		old := fmt.Sprintf("old persisted context %02d %s", i, strings.Repeat("historical-context-", 55))
		_, err = f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: old}}})
		require.NoError(t, err)
		_, err = f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "historical answer " + strings.Repeat("answer-context-", 30)}}})
		require.NoError(t, err)
	}
	if workerOverride {
		err = f.env.sessions.UpdateWorkerReviewerModels(ctx, f.sessID, &session.ModelSlotUpdate{Provider: workerCfg.ID, Model: "session-worker"}, nil)
		require.NoError(t, err)
	}
	require.NoError(t, f.env.messages.SetPinned(ctx, pinID, true))
	prompt := "Summarize the useful details and continue the current task."
	_, err = f.coord.runInternal(ctx, f.sessID, prompt, pinned)
	require.NoError(t, err, "Codex requests=%d worker requests=%d", codexCalls.Load(), workerCalls.Load())
	require.EqualValues(t, 1, workerCalls.Load())

	trace.mu.Lock()
	trace.contextCanceled = ctx.Err() != nil
	stages := append([]string(nil), trace.stages...)
	classes := append([]string(nil), trace.modelClasses...)
	statuses := append([]int(nil), trace.statuses...)
	requestBytes := append([]int(nil), trace.bodyBytes...)
	trace.mu.Unlock()
	if acceptedTool {
		require.Equal(t, []string{"codex-accepted-tool-step", "codex-context-overflow", "worker-summary", "codex-original"}, stages)
		require.Equal(t, http.StatusBadRequest, statuses[1])
		require.Less(t, requestBytes[0], codexBodyLimit, "accepted tool step fits the fake size cap")
		require.Greater(t, requestBytes[1], codexBodyLimit, "accepted result enlarges the actual attempted request beyond the fake size cap")
		require.Less(t, requestBytes[3], codexBodyLimit, "summary makes the actual next body fit the fake size cap")
		require.Less(t, requestBytes[3], requestBytes[1], "summary makes the actual next body smaller")
		require.EqualValues(t, 1, tool.calls.Load())
		require.EqualValues(t, 3, codexCalls.Load())
	} else {
		require.Equal(t, []string{"codex-context-overflow", "worker-summary", "codex-original"}, stages)
		require.Greater(t, requestBytes[0], codexBodyLimit, "actual initial request exceeds exact fake size cap")
		require.Equal(t, http.StatusBadRequest, statuses[0])
		require.Less(t, requestBytes[2], codexBodyLimit, "compaction makes the actual request fit the fake size cap")
		require.Less(t, requestBytes[2], requestBytes[0], "compaction must shrink actual request")
		require.EqualValues(t, 2, codexCalls.Load())
	}
	for i, stage := range stages {
		if stage != "worker-summary" {
			require.Equal(t, "original", classes[i], "the codex continuation uses the configured original model")
		}
	}
	require.True(t, trace.codexModelExact, "decoded Codex model must remain exactly gpt-5-codex")
	if workerOverride {
		require.Equal(t, wantWorker, trace.workerModel)
	} else {
		require.Equal(t, wantWorker, trace.workerModel)
	}
	require.True(t, trace.fixtureAuthOnly)
	require.True(t, trace.localHostsOnly)

	finalSession, err := f.env.sessions.Get(ctx, f.sessID)
	require.NoError(t, err)
	trace.summaryPointerSet = finalSession.SummaryMessageID != ""
	require.NotEmpty(t, finalSession.SummaryMessageID)
	finalRows, err := f.env.messages.List(ctx, f.sessID)
	require.NoError(t, err)
	var summaryPersisted, currentUser, pinnedAuthority, acceptedCall, acceptedResult bool
	for _, row := range finalRows {
		if row.Pinned {
			trace.pins++
		}
		trace.acceptedToolCalls += len(row.ToolCalls())
		summaryPersisted = summaryPersisted || row.ID == finalSession.SummaryMessageID && row.IsSummaryMessage && strings.Contains(row.FullText(), "Persisted summary")
		currentUser = currentUser || row.Role == message.User && row.FullText() == prompt
		pinnedAuthority = pinnedAuthority || row.ID == pinID && row.Pinned && strings.Contains(row.FullText(), "pinned authority")
		for _, call := range row.ToolCalls() {
			acceptedCall = acceptedCall || call.ID == "call_recovery_once" && call.Name == "overflow_once"
		}
		for _, result := range row.ToolResults() {
			acceptedResult = acceptedResult || result.ToolCallID == "call_recovery_once" && strings.HasPrefix(result.Content, "tool-result-")
		}
	}
	trace.ownershipBusy = f.sa.IsSessionBusy(f.sessID)
	require.Equal(t, acceptedTool, acceptedCall)
	require.Equal(t, acceptedTool, acceptedResult)
	expectedAcceptedToolCalls := 0
	if acceptedTool {
		expectedAcceptedToolCalls = 1
	}
	require.Equal(t, expectedAcceptedToolCalls, trace.acceptedToolCalls)
	require.False(t, trace.ownershipBusy)
	require.True(t, trace.summaryPointerSet)
	require.True(t, summaryPersisted)
	require.True(t, currentUser)
	require.True(t, pinnedAuthority)
	require.True(t, len(trace.codexBodies) >= 2)
	resumed := trace.codexBodies[len(trace.codexBodies)-1]
	require.True(t, resumed.summary)
	require.True(t, resumed.pin)
	require.True(t, resumed.user)
	require.Equal(t, acceptedTool, resumed.toolCall)
	require.Equal(t, acceptedTool, resumed.toolResult)
}

func overflowRecoveryReply(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"continued after persisted summary\"}]}],\"usage\":{\"input_tokens\":20,\"output_tokens\":8,\"total_tokens\":28}}}\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
