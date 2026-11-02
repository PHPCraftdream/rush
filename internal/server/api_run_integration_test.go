package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
)

type apiRunIntegrationCoordinator struct {
	agent.Coordinator
	mu      sync.Mutex
	cancel  context.CancelFunc
	busy    bool
	started chan struct{}
	release chan error
}

func newAPIRunIntegrationCoordinator() *apiRunIntegrationCoordinator {
	return &apiRunIntegrationCoordinator{started: make(chan struct{}, 4), release: make(chan error)}
}

func (c *apiRunIntegrationCoordinator) ReserveExclusive(ctx context.Context, _ string) (context.Context, uint64, context.CancelFunc, bool) {
	reserved, cancel := context.WithCancel(ctx)
	return reserved, 1, cancel, true
}

func (*apiRunIntegrationCoordinator) ReleaseExclusive(string, uint64, context.CancelFunc) {}

func (*apiRunIntegrationCoordinator) ResetAutoResumeCounter(string) {}

func (c *apiRunIntegrationCoordinator) IsSessionBusy(string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy
}

func (c *apiRunIntegrationCoordinator) Cancel(string) {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *apiRunIntegrationCoordinator) RunWithReservedOwnership(ctx context.Context, _, _ string, _ uint64, _ context.CancelFunc, onHandoff func(), _, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.mu.Lock()
	c.busy = true
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.mu.Unlock()
	if onHandoff != nil {
		onHandoff()
	}
	c.started <- struct{}{}
	var err error
	select {
	case err = <-c.release:
	case <-runCtx.Done():
		err = runCtx.Err()
	}
	cancel()
	c.mu.Lock()
	c.busy = false
	c.cancel = nil
	c.mu.Unlock()
	return nil, err
}

type apiRunIntegrationHTTPResult struct {
	status int
	header http.Header
	body   []byte
}

func apiRunIntegrationRequest(t *testing.T, client *http.Client, baseURL, method, target, token, body string) apiRunIntegrationHTTPResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, baseURL+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close response: %v/%v", readErr, closeErr)
	}
	return apiRunIntegrationHTTPResult{status: response.StatusCode, header: response.Header.Clone(), body: responseBody}
}

func awaitAPIRunIntegrationStart(t *testing.T, coordinator *apiRunIntegrationCoordinator) {
	t.Helper()
	select {
	case <-coordinator.started:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator did not start within 10 seconds")
	}
}

func TestAPIRunProductionListenerCreatePollAndCancel(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("run-integration", []string{"run", "read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := newAPIRunIntegrationCoordinator()
	coordinator.Coordinator = app.AgentCoordinator
	app.AgentCoordinator = coordinator
	createdSession, err := app.Sessions.Create(t.Context(), "API run integration")
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if stopErr := api.Stop(ctx); stopErr != nil {
			t.Errorf("stop serve API: %v", stopErr)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}

	created := apiRunIntegrationRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+createdSession.ID+"/runs", token, `{"prompt":"complete deterministically"}`)
	if created.status != http.StatusAccepted {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	var run struct {
		ID string `json:"run_id"`
	}
	if err := json.Unmarshal(created.body, &run); err != nil || run.ID == "" {
		t.Fatalf("create response = %s err=%v", created.body, err)
	}
	awaitAPIRunIntegrationStart(t, coordinator)
	select {
	case coordinator.release <- nil:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator completion was not received within 10 seconds")
	}
	polled := apiRunIntegrationRequest(t, client, baseURL, http.MethodGet, "/api/v1/runs/"+run.ID+"?wait=10", token, "")
	var status APIRun
	if polled.status != http.StatusOK || json.Unmarshal(polled.body, &status) != nil || status.State != APIRunSucceeded {
		t.Fatalf("poll = %d %s, want succeeded", polled.status, polled.body)
	}

	created = apiRunIntegrationRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+createdSession.ID+"/runs", token, `{"prompt":"cancel deterministically"}`)
	if created.status != http.StatusAccepted || json.Unmarshal(created.body, &run) != nil || run.ID == "" {
		t.Fatalf("cancel run create = %d %s", created.status, created.body)
	}
	awaitAPIRunIntegrationStart(t, coordinator)
	cancelled := apiRunIntegrationRequest(t, client, baseURL, http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", token, "")
	if cancelled.status != http.StatusAccepted {
		t.Fatalf("cancel = %d %s", cancelled.status, cancelled.body)
	}
	finished := apiRunIntegrationRequest(t, client, baseURL, http.MethodGet, "/api/v1/runs/"+run.ID+"?wait=10", token, "")
	var cancelledRun APIRun
	if finished.status != http.StatusOK || json.Unmarshal(finished.body, &cancelledRun) != nil || cancelledRun.State != APIRunCancelled {
		t.Fatalf("cancelled run = %d %s", finished.status, finished.body)
	}
	cancelledAgain := apiRunIntegrationRequest(t, client, baseURL, http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", token, "")
	var finalRun APIRun
	if cancelledAgain.status != http.StatusOK || json.Unmarshal(cancelledAgain.body, &finalRun) != nil || finalRun.State != APIRunCancelled {
		t.Fatalf("repeat cancel = %d %s, want final cancelled DTO", cancelledAgain.status, cancelledAgain.body)
	}
	secret := token[strings.IndexByte(token, '.')+1:]
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), secret) {
		t.Fatal("plaintext token appeared in captured logs")
	}
}
