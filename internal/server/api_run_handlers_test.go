package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type apiRunHandlerStore struct {
	item    session.Session
	foreign bool
}

func (s *apiRunHandlerStore) CreateSession(*http.Request, string, message.Origin) (session.Session, error) {
	return session.Session{}, nil
}

func (s *apiRunHandlerStore) ForkSession(*http.Request, string, string) (session.Session, error) {
	return session.Session{}, nil
}
func (s *apiRunHandlerStore) DeleteSession(*http.Request, string) error { return nil }
func (s *apiRunHandlerStore) GetSession(_ *http.Request, id string) (session.Session, error) {
	if s.item.ID == "" || s.item.ID != id {
		return session.Session{}, sql.ErrNoRows
	}
	return s.item, nil
}
func (*apiRunHandlerStore) SessionBusy(string) bool            { return false }
func (s *apiRunHandlerStore) OwnsSession(session.Session) bool { return !s.foreign }

type apiRunHandlerBackend struct {
	mu       sync.Mutex
	starts   int
	busy     bool
	cancel   bool
	inject   bool
	injected []string
	started  chan struct{}
	outcomes chan RunOutcome
	startErr error
}

func newAPIRunHandlerBackend() *apiRunHandlerBackend {
	return &apiRunHandlerBackend{started: make(chan struct{}, 8), outcomes: make(chan RunOutcome, 8), cancel: true, inject: true}
}

func (b *apiRunHandlerBackend) Start(_ context.Context, _, _, _, _, _ string) (<-chan RunOutcome, error) {
	if b.startErr != nil {
		return nil, b.startErr
	}
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	b.started <- struct{}{}
	return b.outcomes, nil
}
func (b *apiRunHandlerBackend) Cancel(string, string) bool { return b.cancel }
func (b *apiRunHandlerBackend) Inject(_, _, mode string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.injected = append(b.injected, mode)
	return b.inject
}
func (b *apiRunHandlerBackend) Busy(string) bool { return b.busy }

type apiRunHTTPResult struct {
	status int
	header http.Header
	body   []byte
}

func apiRunRequest(t *testing.T, h http.Handler, method, target, body string, principal Principal, ctx context.Context) apiRunHTTPResult {
	return apiRunRequestWithKey(t, h, method, target, body, principal, ctx, "")
}

func apiRunRequestWithKey(t *testing.T, h http.Handler, method, target, body string, principal Principal, ctx context.Context, key string) apiRunHTTPResult {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r = r.WithContext(WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return apiRunHTTPResult{status: w.Code, header: w.Header().Clone(), body: append([]byte(nil), w.Body.Bytes()...)}
}

func apiRunHandlerEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
}

func apiRunHandler(store APIWriteStore, backend APIRunBackend) http.Handler {
	return NewAPIHandler(apiRunWriteRoutes(store, backend))
}

func awaitAPIRunStart(t *testing.T, backend *apiRunHandlerBackend) {
	t.Helper()
	select {
	case <-backend.started:
	case <-time.After(10 * time.Second):
		t.Fatal("backend Start was not called within 10 seconds")
	}
}

func releaseAPIRunOnCleanup(t *testing.T, backend *apiRunHandlerBackend) {
	t.Helper()
	t.Cleanup(func() {
		backend.outcomes <- RunOutcome{State: APIRunSucceeded}
	})
}

func TestAPIRunCreateResponseAndReplay(t *testing.T) {
	apiRunHandlerEnv(t)
	store := &apiRunHandlerStore{item: session.Session{ID: "session-1"}}
	backend := newAPIRunHandlerBackend()
	h := apiRunHandler(store, backend)
	principal := Principal{TokenID: "token-a", Scopes: []string{"run"}}
	first := apiRunRequestWithKey(t, h, http.MethodPost, "/api/v1/sessions/session-1/runs", `{"prompt":"hello"}`, principal, nil, "replay-key")
	if first.status != http.StatusAccepted || first.header.Get("Location") == "" {
		t.Fatalf("status=%d location=%q body=%s", first.status, first.header.Get("Location"), first.body)
	}
	var response struct {
		RunID     string      `json:"run_id"`
		SessionID string      `json:"session_id"`
		State     APIRunState `json:"state"`
	}
	if err := json.Unmarshal(first.body, &response); err != nil || response.RunID == "" || response.SessionID != "session-1" || response.State != APIRunRunning {
		t.Fatalf("response=%s err=%v", first.body, err)
	}
	wantBody, err := json.Marshal(response)
	if err != nil || string(first.body) != string(append(wantBody, '\n')) {
		t.Fatalf("body=%s want exact JSON=%s\\n err=%v", first.body, wantBody, err)
	}
	if got, want := first.header.Get("Location"), "/api/v1/runs/"+response.RunID; got != want {
		t.Fatalf("Location=%q want %q", got, want)
	}
	replay := apiRunRequestWithKey(t, h, http.MethodPost, "/api/v1/sessions/session-1/runs", `{"prompt":"hello"}`, principal, nil, "replay-key")
	if replay.status != http.StatusAccepted || replay.header.Get("Location") != first.header.Get("Location") || string(replay.body) != string(first.body) {
		t.Fatalf("replay status=%d location=%q body=%s", replay.status, replay.header.Get("Location"), replay.body)
	}
	conflict := apiRunRequestWithKey(t, h, http.MethodPost, "/api/v1/sessions/session-1/runs", `{"prompt":"different"}`, principal, nil, "replay-key")
	if conflict.status != http.StatusUnprocessableEntity {
		t.Fatalf("same-key different-body status=%d body=%s", conflict.status, conflict.body)
	}
	awaitAPIRunStart(t, backend)
	releaseAPIRunOnCleanup(t, backend)
	backend.mu.Lock()
	starts := backend.starts
	backend.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	backend.outcomes <- RunOutcome{State: APIRunSucceeded, ResultMessageID: "finished"}
}

func TestAPIRunCreateValidationAndConflicts(t *testing.T) {
	apiRunHandlerEnv(t)
	cases := []struct {
		name, body string
		status     int
	}{
		{"empty prompt", `{"prompt":"  "}`, 400},
		{"oversize", `{"prompt":"` + strings.Repeat("x", apiRunMaxFieldBytes+1) + `"}`, 400},
		{"unknown role", `{"prompt":"x","role":"not-a-role"}`, 400},
		{"unknown field", `{"prompt":"x","extra":true}`, 400},
		{"bad JSON", `{`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &apiRunHandlerStore{item: session.Session{ID: "s"}}
			backend := newAPIRunHandlerBackend()
			got := apiRunRequest(t, apiRunHandler(store, backend), http.MethodPost, "/api/v1/sessions/s/runs", tc.body, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
			if got.status != tc.status {
				t.Fatalf("status=%d body=%s", got.status, got.body)
			}
			backend.mu.Lock()
			starts := backend.starts
			backend.mu.Unlock()
			if starts != 0 {
				t.Fatalf("invalid request started %d runs", starts)
			}
		})
	}
	waitBackend := newAPIRunHandlerBackend()
	waitHandler := apiRunHandler(&apiRunHandlerStore{item: session.Session{ID: "s"}}, waitBackend)
	waitPrincipal := Principal{TokenID: "t", Scopes: []string{"run", "read"}}
	created := apiRunRequest(t, waitHandler, http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, waitPrincipal, nil)
	var activeRun struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &activeRun); err != nil || created.status != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s err=%v", created.status, created.body, err)
	}
	awaitAPIRunStart(t, waitBackend)
	releaseAPIRunOnCleanup(t, waitBackend)
	got := apiRunRequest(t, waitHandler, http.MethodGet, "/api/v1/runs/"+activeRun.RunID+"?wait=31", "", waitPrincipal, nil)
	if got.status != http.StatusBadRequest {
		t.Fatalf("wait cap status=%d body=%s", got.status, got.body)
	}
	waitBackend.outcomes <- RunOutcome{State: APIRunSucceeded}
	store := &apiRunHandlerStore{item: session.Session{ID: "s"}}
	busy := newAPIRunHandlerBackend()
	busy.busy = true
	got = apiRunRequest(t, apiRunHandler(store, busy), http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
	if got.status != http.StatusConflict {
		t.Fatalf("busy status=%d", got.status)
	}
	got = apiRunRequest(t, apiRunHandler(&apiRunHandlerStore{}, newAPIRunHandlerBackend()), http.MethodPost, "/api/v1/sessions/missing/runs", `{"prompt":"x"}`, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
	if got.status != http.StatusNotFound {
		t.Fatalf("missing status=%d", got.status)
	}
}

func TestAPIRunWaitAndCancelEndpoints(t *testing.T) {
	apiRunHandlerEnv(t)
	store := &apiRunHandlerStore{item: session.Session{ID: "s"}}
	backend := newAPIRunHandlerBackend()
	h := apiRunHandler(store, backend)
	p := Principal{TokenID: "t", Scopes: []string{"run"}}
	created := apiRunRequest(t, h, http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, p, nil)
	var run struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &run); err != nil || run.RunID == "" {
		t.Fatalf("create body=%s err=%v", created.body, err)
	}
	awaitAPIRunStart(t, backend)
	releaseAPIRunOnCleanup(t, backend)
	ctx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	waitResult := make(chan apiRunHTTPResult, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs/"+run.RunID+"?wait=30", nil)
		if err != nil {
			waitResult <- apiRunHTTPResult{}
			return
		}
		request = request.WithContext(WithPrincipal(request.Context(), Principal{TokenID: "t", Scopes: []string{"read"}}))
		writer := httptest.NewRecorder()
		h.ServeHTTP(writer, request)
		waitResult <- apiRunHTTPResult{status: writer.Code, header: writer.Header().Clone(), body: append([]byte(nil), writer.Body.Bytes()...)}
	}()
	select {
	case got := <-waitResult:
		if got.status != http.StatusOK {
			t.Fatalf("cancelled-context wait status=%d", got.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled-context wait did not return promptly")
	}
	cancelWait()
	cancelled := apiRunRequest(t, h, http.MethodPost, "/api/v1/runs/"+run.RunID+"/cancel", "", p, nil)
	if cancelled.status != http.StatusAccepted {
		t.Fatalf("cancel status=%d body=%s", cancelled.status, cancelled.body)
	}
	var cancelResponse struct {
		State APIRunState `json:"state"`
	}
	if err := json.Unmarshal(cancelled.body, &cancelResponse); err != nil || cancelResponse.State != APIRunRunning {
		t.Fatalf("cancel response=%s err=%v", cancelled.body, err)
	}
	backend.outcomes <- RunOutcome{State: APIRunCancelled, ErrorCode: "cancelled", ErrorSummary: "run cancelled"}
	finished := apiRunRequest(t, h, http.MethodGet, "/api/v1/runs/"+run.RunID+"?wait=1", "", p, nil)
	if finished.status != http.StatusOK {
		t.Fatalf("finished get status=%d", finished.status)
	}
	var finalRun APIRun
	if err := json.Unmarshal(finished.body, &finalRun); err != nil || finalRun.State != APIRunCancelled {
		t.Fatalf("final run=%s err=%v", finished.body, err)
	}
	finishedCancel := apiRunRequest(t, h, http.MethodPost, "/api/v1/runs/"+run.RunID+"/cancel", "", p, nil)
	if finishedCancel.status != http.StatusOK {
		t.Fatalf("finished cancel status=%d", finishedCancel.status)
	}
	unknown := apiRunRequest(t, h, http.MethodPost, "/api/v1/runs/unknown/cancel", "", p, nil)
	if unknown.status != http.StatusNotFound {
		t.Fatalf("unknown cancel status=%d", unknown.status)
	}
}

func TestAPIRunWaitReturnsOnCompletion(t *testing.T) {
	apiRunHandlerEnv(t)
	backend := newAPIRunHandlerBackend()
	h := apiRunHandler(&apiRunHandlerStore{item: session.Session{ID: "s"}}, backend)
	p := Principal{TokenID: "wait-token", Scopes: []string{"run"}}
	created := apiRunRequest(t, h, http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, p, nil)
	var run struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &run); err != nil || created.status != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s err=%v", created.status, created.body, err)
	}
	awaitAPIRunStart(t, backend)
	releaseAPIRunOnCleanup(t, backend)
	type result struct {
		response apiRunHTTPResult
		elapsed  time.Duration
	}
	finished := make(chan result, 1)
	requestStarted := make(chan struct{})
	started := time.Now()
	go func() {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/runs/"+run.RunID+"?wait=30", nil)
		if err != nil {
			finished <- result{response: apiRunHTTPResult{status: 0}}
			return
		}
		request = request.WithContext(WithPrincipal(request.Context(), Principal{TokenID: "wait-token", Scopes: []string{"read"}}))
		writer := httptest.NewRecorder()
		close(requestStarted)
		h.ServeHTTP(writer, request)
		finished <- result{response: apiRunHTTPResult{status: writer.Code, header: writer.Header().Clone(), body: append([]byte(nil), writer.Body.Bytes()...)}, elapsed: time.Since(started)}
	}()
	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("wait request did not start")
	}
	backend.outcomes <- RunOutcome{State: APIRunSucceeded, ResultMessageID: "m"}
	select {
	case got := <-finished:
		var dto APIRun
		if err := json.Unmarshal(got.response.body, &dto); err != nil || dto.MessageID != "m" || dto.State != APIRunSucceeded {
			t.Fatalf("wait DTO=%s err=%v", got.response.body, err)
		}
		if got.response.status != http.StatusOK || got.elapsed >= 30*time.Second {
			t.Fatalf("status=%d elapsed=%s", got.response.status, got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not return after completion")
	}
}

func TestAPIRunOwnershipAndReadScope(t *testing.T) {
	apiRunHandlerEnv(t)
	backend := newAPIRunHandlerBackend()
	store := &apiRunHandlerStore{item: session.Session{ID: "s"}}
	h := apiRunHandler(store, backend)
	tokenA := Principal{TokenID: "a", Scopes: []string{"run", "read"}}
	tokenB := Principal{TokenID: "b", Scopes: []string{"run", "read"}}
	admin := Principal{TokenID: "admin", Scopes: []string{"admin"}}
	created := apiRunRequest(t, h, http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, tokenA, nil)
	var run struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &run); err != nil {
		t.Fatal(err)
	}
	awaitAPIRunStart(t, backend)
	releaseAPIRunOnCleanup(t, backend)
	for _, tc := range []struct{ name, method, target string }{
		{"get", http.MethodGet, "/api/v1/runs/" + run.RunID},
		{"cancel", http.MethodPost, "/api/v1/runs/" + run.RunID + "/cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := apiRunRequest(t, h, tc.method, tc.target, "", tokenB, nil)
			if got.status != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", got.status, got.body)
			}
		})
	}
	if got := apiRunRequest(t, h, http.MethodGet, "/api/v1/runs/"+run.RunID, "", admin, nil); got.status != http.StatusOK {
		t.Fatalf("admin status=%d", got.status)
	}
	readOnly := Principal{TokenID: "read-only", Scopes: []string{"read"}}
	for _, target := range []string{"/api/v1/sessions/s/runs", "/api/v1/runs/" + run.RunID + "/cancel", "/api/v1/sessions/s/inject"} {
		got := apiRunRequest(t, h, http.MethodPost, target, `{"prompt":"x","message":"x"}`, readOnly, nil)
		if got.status != http.StatusForbidden {
			t.Fatalf("%s status=%d body=%s", target, got.status, got.body)
		}
	}
	backend.outcomes <- RunOutcome{State: APIRunSucceeded}
}

func TestAPIRunInjectModes(t *testing.T) {
	apiRunHandlerEnv(t)
	backend := newAPIRunHandlerBackend()
	h := apiRunHandler(&apiRunHandlerStore{item: session.Session{ID: "s"}}, backend)
	principal := Principal{TokenID: "t", Scopes: []string{"run"}}
	created := apiRunRequest(t, h, http.MethodPost, "/api/v1/sessions/s/runs", `{"prompt":"x"}`, principal, nil)
	if created.status != http.StatusAccepted {
		t.Fatalf("create status=%d", created.status)
	}
	awaitAPIRunStart(t, backend)
	releaseAPIRunOnCleanup(t, backend)
	for _, mode := range []string{"merge", "interrupt"} {
		body := `{"message":"reply"}`
		if mode != "merge" {
			body = `{"message":"reply","mode":"` + mode + `"}`
		}
		got := apiRunRequest(t, h, http.MethodPost, "/api/v1/sessions/s/inject", body, principal, nil)
		if got.status != http.StatusAccepted {
			t.Fatalf("mode %s status=%d body=%s", mode, got.status, got.body)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.injected) != 2 || backend.injected[0] != "merge" || backend.injected[1] != "interrupt" {
		t.Fatalf("injected modes=%v", backend.injected)
	}
	backend.outcomes <- RunOutcome{State: APIRunSucceeded}
}

func TestAPIRunInjectRequiresActiveRun(t *testing.T) {
	apiRunHandlerEnv(t)
	store := &apiRunHandlerStore{item: session.Session{ID: "s"}}
	got := apiRunRequest(t, apiRunHandler(store, newAPIRunHandlerBackend()), http.MethodPost, "/api/v1/sessions/s/inject", `{"message":"reply"}`, Principal{TokenID: "t", Scopes: []string{"run"}}, nil)
	if got.status != http.StatusConflict {
		t.Fatalf("no active run status=%d", got.status)
	}
}
