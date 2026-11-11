package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestStandaloneSummarizer_DefaultWorkerOverrideAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		want     string
		override bool
	}{
		{name: "worker beats smart and fast", want: "worker-model"},
		{name: "session worker override wins", want: "session-worker", override: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &standaloneSummaryProbe{}
			server := standaloneSummaryServer(t, probe, standaloneSummaryOK)
			ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)
			probe.configureEndpoint(t, server.URL)
			probe.installFailureDiagnostics(t, ctx)
			probe.setStage("worker-summary")
			if tc.override {
				require.NoError(t, sessions.UpdateWorkerReviewerModels(ctx, sessionID, &session.ModelSlotUpdate{Provider: "probe", Model: "session-worker"}, nil))
			}

			summarizer := NewStandaloneSummarizer(standaloneSummaryConfig(server.URL, true, dataDir), sessions, messages)
			err := summarizer.SummarizeWithRole(ctx, sessionID, config.SelectedModelTypeWorker)
			probe.recordSummaryState(ctx, sessions, messages, sessionID, "returned", err == nil)
			require.NoError(t, err)
			probe.requireRequest(t, 1, standaloneModelClass(tc.want), http.StatusOK, standaloneSummaryOK)

			after, err := messages.List(ctx, sessionID)
			require.NoError(t, err)
			require.Len(t, after, 2)
			summary := standaloneFindSummary(t, after)
			require.Equal(t, tc.want, summary.Model)
			pinned := standaloneFindMessage(t, after, seeded[0].ID)
			require.True(t, pinned.Pinned)
			updated, err := sessions.Get(ctx, sessionID)
			require.NoError(t, err)
			require.Equal(t, summary.ID, updated.SummaryMessageID)
		})
	}
}

func TestStandaloneSummarizer_MissingWorkerDoesNotCallProvider(t *testing.T) {
	probe := &standaloneSummaryProbe{}
	server := standaloneSummaryServer(t, probe, standaloneSummaryOK)
	ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)
	probe.configureEndpoint(t, server.URL)
	probe.installFailureDiagnostics(t, ctx)
	probe.setStage("missing-worker")

	summarizer := NewStandaloneSummarizer(standaloneSummaryConfig(server.URL, false, dataDir), sessions, messages)
	err := summarizer.SummarizeWithRole(ctx, sessionID, config.SelectedModelTypeWorker)
	probe.recordSummaryState(ctx, sessions, messages, sessionID, "not-started", false)
	require.ErrorContains(t, err, "worker model is unavailable")
	probe.requireNoRequest(t)
	standaloneRequireHistory(t, ctx, sessions, messages, sessionID, seeded)
}

func TestStandaloneSummarizer_BusyAdmissionAndOSLockRefuseWithoutQueueOrWrite(t *testing.T) {
	for _, mailboxBusy := range []bool{true, false} {
		t.Run(fmt.Sprintf("mailbox-busy-%t", mailboxBusy), func(t *testing.T) {
			probe := &standaloneSummaryProbe{}
			server := standaloneSummaryServer(t, probe, standaloneSummaryOK)
			ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)
			probe.configureEndpoint(t, server.URL)
			probe.installFailureDiagnostics(t, ctx)
			probe.setStage("busy-refusal")
			store := standaloneSummaryConfig(server.URL, true, dataDir)
			cfg, _ := store.Snapshot()
			coord := &coordinator{cfg: store, sessions: sessions, messages: messages}
			model, _, err := coord.buildModelsFromCfg(ctx, cfg, config.SelectedModel{Provider: "probe", Model: "worker-model"}, config.SelectedModel{Provider: "probe", Model: "worker-model"}, false)
			probe.recordState(false, false, false)
			require.NoError(t, err)
			standaloneAgent := NewSessionAgent(SessionAgentOptions{SmartModel: model, FastModel: model, Sessions: sessions, Messages: messages, DataDirectory: cfg.Options.DataDirectory}).(*sessionAgent)
			if mailboxBusy {
				_, ok := standaloneAgent.getMailbox(sessionID).beginCompact(func() {})
				require.True(t, ok)
			} else {
				lock, lockErr := session.TryAcquireSessionLock(cfg.Options.DataDirectory, sessionID)
				probe.recordState(false, false, false)
				require.NoError(t, lockErr)
				defer lock.Release()
			}

			err = standaloneAgent.SummarizeNow(ctx, sessionID, &SummarizeSnapshot{model: model})
			probe.recordState(standaloneAgent.IsSessionBusy(sessionID), standaloneAgent.SummarizeQueued(sessionID), false)
			require.Error(t, err)
			require.False(t, standaloneAgent.SummarizeQueued(sessionID))
			probe.requireNoRequest(t)
			standaloneRequireHistory(t, ctx, sessions, messages, sessionID, seeded)
		})
	}
}

func TestStandaloneSummarizer_ProviderFailurePreservesHistory(t *testing.T) {
	probe := &standaloneSummaryProbe{}
	server := standaloneSummaryServer(t, probe, standaloneSummaryHTTPFailure)
	ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)
	probe.configureEndpoint(t, server.URL)
	probe.installFailureDiagnostics(t, ctx)
	probe.setStage("provider-failure")

	summarizer := NewStandaloneSummarizer(standaloneSummaryConfig(server.URL, true, dataDir), sessions, messages)
	err := summarizer.SummarizeWithRole(ctx, sessionID, config.SelectedModelTypeWorker)
	checkCtx, checkCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer checkCancel()
	probe.recordSummaryState(checkCtx, sessions, messages, sessionID, "provider-failed", false)
	require.Error(t, err)
	probe.requireRequest(t, 1, "worker", http.StatusBadRequest, standaloneSummaryHTTPFailure)
	standaloneRequireHistory(t, checkCtx, sessions, messages, sessionID, seeded)
}

func TestStandaloneSummarizer_CancellationPreservesHistory(t *testing.T) {
	probe := &standaloneSummaryProbe{}
	entered := make(chan struct{})
	server := standaloneSummaryBlockingServer(t, probe, entered)
	ctx, sessions, messages, sessionID, seeded, dataDir := newStandaloneSummaryFixture(t)
	probe.configureEndpoint(t, server.URL)
	probe.installFailureDiagnostics(t, ctx)
	probe.setStage("cancel-provider-stream")
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	summarizer := NewStandaloneSummarizer(standaloneSummaryConfig(server.URL, true, dataDir), sessions, messages)
	probe.setContext(callCtx)
	result := make(chan error, 1)
	go func() { result <- summarizer.SummarizeWithRole(callCtx, sessionID, config.SelectedModelTypeWorker) }()

	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("provider was not reached; requests=%d model_class=%q", probe.calls.Load(), probe.modelClass())
	}
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatalf("summarize did not stop after cancellation; requests=%d model_class=%q", probe.calls.Load(), probe.modelClass())
	}
	checkCtx, checkCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer checkCancel()
	probe.recordSummaryState(checkCtx, sessions, messages, sessionID, "canceled", false)
	probe.ctxCanceled.Store(callCtx.Err() != nil)
	require.ErrorIs(t, err, context.Canceled)
	probe.requireRequest(t, 1, "worker", http.StatusOK, standaloneSummaryHoldStream)
	standaloneRequireHistory(t, checkCtx, sessions, messages, sessionID, seeded)
}

type standaloneSummaryStage string

const (
	standaloneSummaryOK          standaloneSummaryStage = "summary-complete"
	standaloneSummaryHTTPFailure standaloneSummaryStage = "http-400"
	standaloneSummaryHoldStream  standaloneSummaryStage = "stream-waiting"
)

type standaloneSummaryProbe struct {
	calls             atomic.Int32
	modelValue        atomic.Value
	requestModelValue atomic.Value
	requestStage      atomic.Value
	methodPath        atomic.Value
	actualHost        atomic.Value
	endpoint          atomic.Value
	stage             atomic.Value
	ownership         atomic.Value
	summaryState      atomic.Value
	authMatches       atomic.Bool
	localEndpoint     atomic.Bool
	responseCode      atomic.Int32
	expectedHost      atomic.Value
	callCtx           context.Context
	ctxMu             sync.RWMutex
	ctxCanceled       atomic.Bool
	ctxHasDeadline    atomic.Bool
}

func (p *standaloneSummaryProbe) initialize() {
	p.modelValue.Store("")
	p.requestModelValue.Store("")
	p.requestStage.Store(standaloneSummaryStage("unknown"))
	p.methodPath.Store("")
	p.actualHost.Store("")
	p.endpoint.Store("")
	p.stage.Store("setup")
	p.ownership.Store("unknown")
	p.summaryState.Store("unknown")
	p.expectedHost.Store("")
	p.authMatches.Store(false)
	p.localEndpoint.Store(false)
	p.responseCode.Store(0)
}

func (p *standaloneSummaryProbe) configureEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	require.NoError(t, err)
	local := false
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		local = ip.IsLoopback()
	} else {
		local = strings.EqualFold(parsed.Hostname(), "localhost")
	}
	require.True(t, local, "fake endpoint must resolve to loopback")
	require.Empty(t, parsed.User)
	require.Empty(t, parsed.RawQuery)
	p.localEndpoint.Store(local)
	p.endpoint.Store(parsed.Host + parsed.EscapedPath())
	p.expectedHost.Store(parsed.Host)
}

func installStandaloneSummaryDiagnostics(t *testing.T, probe *standaloneSummaryProbe) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		probe.ctxMu.RLock()
		callCtx := probe.callCtx
		probe.ctxMu.RUnlock()
		deadline := probe.ctxHasDeadline.Load()
		canceled := probe.ctxCanceled.Load()
		if callCtx != nil {
			canceled = canceled || callCtx.Err() != nil
		}
		t.Logf("redacted summarize diagnostic: stage=%s requests=%d model_class=%q method_path=%q endpoint=%q local_endpoint=%t actual_host_matches=%t fixture_auth_match=%t status=%d ctx_canceled=%t ctx_deadline=%t ownership=%q summary_state=%q",
			probe.stage.Load().(string), probe.calls.Load(), probe.modelClass(), probe.methodPath.Load().(string), probe.endpoint.Load().(string), probe.localEndpoint.Load(), probe.actualHost.Load().(string) == probe.expectedHost.Load().(string), probe.authMatches.Load(), probe.responseCode.Load(), canceled, deadline, probe.ownership.Load().(string), probe.summaryState.Load().(string))
	})
}

func (p *standaloneSummaryProbe) installFailureDiagnostics(t *testing.T, ctx context.Context) {
	t.Helper()
	p.setContext(ctx)
	_, hasDeadline := ctx.Deadline()
	p.ctxHasDeadline.Store(hasDeadline)
	p.ctxCanceled.Store(ctx.Err() != nil)
}

func (p *standaloneSummaryProbe) setContext(ctx context.Context) {
	p.ctxMu.Lock()
	p.callCtx = ctx
	p.ctxMu.Unlock()
}

func (p *standaloneSummaryProbe) setStage(stage string) {
	p.stage.Store(stage)
}

func (p *standaloneSummaryProbe) recordState(busy, queued, hasSummary bool) {
	p.ownership.Store(fmt.Sprintf("busy=%t queued=%t", busy, queued))
	p.summaryState.Store(fmt.Sprintf("summary_id_empty=%t", !hasSummary))
}

func (p *standaloneSummaryProbe) recordSummaryState(ctx context.Context, sessions session.Service, messages message.Service, sessionID, stage string, success bool) {
	_, hasDeadline := ctx.Deadline()
	p.ctxHasDeadline.Store(hasDeadline)
	p.ctxCanceled.Store(ctx.Err() != nil)
	p.setStage(stage)
	sess, err := sessions.Get(ctx, sessionID)
	hasSummary := err == nil && sess.SummaryMessageID != ""
	found := false
	if err == nil {
		rows, listErr := messages.List(ctx, sessionID)
		if listErr == nil {
			for _, row := range rows {
				if row.IsSummaryMessage {
					found = true
					break
				}
			}
		}
	}
	p.summaryState.Store(fmt.Sprintf("pointer_set=%t summary_message=%t success=%t db_observed=%t", hasSummary, found, success, err == nil))
}

func (p *standaloneSummaryProbe) recordRequest(r *http.Request, stage standaloneSummaryStage) {
	p.calls.Add(1)
	var request struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	p.modelValue.Store(standaloneModelClass(request.Model))
	p.requestModelValue.Store(standaloneFixtureModel(request.Model))
	p.methodPath.Store(r.Method + " " + r.URL.EscapedPath())
	p.actualHost.Store(r.Host)
	p.authMatches.Store(r.Header.Get("Authorization") == "Bearer test")
	if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		p.responseCode.Store(int32(standaloneStageStatus(stage)))
	}
	p.requestStage.Store(stage)
	p.stage.Store(string(stage))
}

func standaloneStageStatus(stage standaloneSummaryStage) int {
	if stage == standaloneSummaryHTTPFailure {
		return http.StatusBadRequest
	}
	return http.StatusOK
}

func standaloneModelClass(model string) string {
	switch model {
	case "worker-model":
		return "worker"
	case "session-worker":
		return "session-worker"
	default:
		return "other"
	}
}

func standaloneFixtureModel(model string) string {
	switch model {
	case "worker-model", "session-worker", "smart-model", "fast-model":
		return model
	default:
		return ""
	}
}

func (p *standaloneSummaryProbe) model() string {
	value := p.requestModelValue.Load()
	if value == nil {
		return ""
	}
	return value.(string)
}

func (p *standaloneSummaryProbe) modelClass() string {
	value := p.modelValue.Load()
	if value == nil {
		return ""
	}
	return value.(string)
}

func (p *standaloneSummaryProbe) requireRequest(t *testing.T, count int32, modelClass string, status int, stage standaloneSummaryStage) {
	t.Helper()
	require.Equal(t, count, p.calls.Load())
	require.Equal(t, modelClass, p.modelClass())
	require.Equal(t, "POST /v1/chat/completions", p.methodPath.Load().(string))
	require.True(t, p.localEndpoint.Load())
	require.Equal(t, p.expectedHost.Load().(string), p.actualHost.Load().(string))
	require.True(t, p.authMatches.Load(), "request used fixture authentication")
	require.Equal(t, int32(status), p.responseCode.Load())
	require.Equal(t, stage, p.requestStage.Load().(standaloneSummaryStage))
}

func (p *standaloneSummaryProbe) requireNoRequest(t *testing.T) {
	t.Helper()
	require.Zero(t, p.calls.Load())
	require.Empty(t, p.modelClass())
}

func newStandaloneSummaryFixture(t *testing.T) (context.Context, session.Service, message.Service, string, []message.Message, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	t.Cleanup(cancel)
	isolateAllGlobalConfigPaths(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	sessions, messages := session.NewService(q, conn), message.NewService(q)
	sess, err := sessions.CreateWithID(ctx, "role-session", "role test")
	require.NoError(t, err)
	seeded := standaloneSeedHistory(t, ctx, messages, sess.ID)
	return ctx, sessions, messages, sess.ID, seeded, dataDir
}

func standaloneSeedHistory(t *testing.T, ctx context.Context, messages message.Service, sessionID string) []message.Message {
	t.Helper()
	seeded := make([]message.Message, 0, 4)
	for i := 0; i < 4; i++ {
		role := message.User
		if i%2 == 1 {
			role = message.Assistant
		}
		msg, err := messages.Create(ctx, sessionID, message.CreateMessageParams{Role: role, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("history %d", i)}}})
		require.NoError(t, err)
		if role == message.Assistant {
			msg.AddFinish(message.FinishReasonEndTurn, "", "")
			require.NoError(t, messages.Update(ctx, msg.Clone()))
		}
		seeded = append(seeded, msg)
	}
	require.NoError(t, messages.SetPinned(ctx, seeded[0].ID, true))
	seeded[0].Pinned = true
	return seeded
}

func standaloneRequireHistory(t *testing.T, ctx context.Context, sessions session.Service, messages message.Service, sessionID string, seeded []message.Message) {
	t.Helper()
	sess, err := sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, sess.SummaryMessageID)
	rows, err := messages.List(ctx, sessionID)
	require.NoError(t, err)
	byID := make(map[string]message.Message, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, want := range seeded {
		got, ok := byID[want.ID]
		require.True(t, ok, "history message %s was lost", want.ID)
		require.Equal(t, want.Role, got.Role)
		require.Equal(t, want.Content().Text, got.Content().Text)
		require.Equal(t, want.Pinned, got.Pinned)
	}
}

func standaloneFindSummary(t *testing.T, rows []message.Message) message.Message {
	t.Helper()
	for _, row := range rows {
		if row.IsSummaryMessage {
			return row
		}
	}
	t.Fatal("summary message not found")
	return message.Message{}
}

func standaloneFindMessage(t *testing.T, rows []message.Message, id string) message.Message {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("message %s not found", id)
	return message.Message{}
}

func standaloneSummaryConfig(baseURL string, worker bool, dataDir string) *config.ConfigStore {
	models := map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "probe", Model: "smart-model"},
		config.SelectedModelTypeFast:  {Provider: "probe", Model: "fast-model"},
	}
	if worker {
		models[config.SelectedModelTypeWorker] = config.SelectedModel{Provider: "probe", Model: "worker-model"}
	}
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig](), Options: &config.Options{DataDirectory: dataDir}, Models: models}
	cfg.Providers.Set("probe", config.ProviderConfig{ID: "probe", Type: openaicompat.Name, BaseURL: strings.TrimRight(baseURL, "/") + "/v1", APIKey: "test", Models: []catwalk.Model{{ID: "smart-model"}, {ID: "fast-model"}, {ID: "worker-model"}, {ID: "session-worker"}}})
	return config.NewTestStoreWithResolver(cfg, config.IdentityResolver())
}

func standaloneSummaryServer(t *testing.T, probe *standaloneSummaryProbe, stages ...standaloneSummaryStage) *httptest.Server {
	t.Helper()
	stage := standaloneSummaryOK
	if len(stages) > 0 {
		stage = stages[0]
	}
	probe.initialize()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.recordRequest(r, stage)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			probe.responseCode.Store(http.StatusNotFound)
			http.NotFound(w, r)
			return
		}
		if stage == standaloneSummaryHTTPFailure {
			probe.responseCode.Store(http.StatusBadRequest)
			http.Error(w, `{"error":{"message":"injected summary failure"}}`, http.StatusBadRequest)
			return
		}
		if stage == standaloneSummaryHoldStream {
			probe.responseCode.Store(http.StatusOK)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		probe.responseCode.Store(http.StatusOK)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"summary\"},\"finish_reason\":null}]}\n\n", probe.requestModel())
		fmt.Fprintf(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n", probe.requestModel())
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	installStandaloneSummaryDiagnostics(t, probe)
	return server
}

func (p *standaloneSummaryProbe) requestModel() string {
	value := p.requestModelValue.Load()
	if value == nil {
		return ""
	}
	return value.(string)
}

func standaloneSummaryBlockingServer(t *testing.T, probe *standaloneSummaryProbe, entered chan struct{}) *httptest.Server {
	t.Helper()
	probe.initialize()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe.recordRequest(r, standaloneSummaryHoldStream)
		probe.responseCode.Store(http.StatusOK)
		select {
		case <-entered:
		default:
			close(entered)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	installStandaloneSummaryDiagnostics(t, probe)
	return server
}
