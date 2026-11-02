package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
)

type apiPermissionLogBuffer struct {
	mu sync.Mutex
	strings.Builder
}

func (b *apiPermissionLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Builder.Write(p)
}

func (b *apiPermissionLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Builder.String()
}

type apiPermissionListenerCoordinator struct {
	agent.Coordinator
	app     *appPkg.App
	started chan struct{}
	results chan error
	cleaned chan struct{}
	mu      sync.Mutex
	busy    bool
}

func (c *apiPermissionListenerCoordinator) ReserveExclusive(ctx context.Context, _ string) (context.Context, uint64, context.CancelFunc, bool) {
	reserved, cancel := context.WithCancel(ctx)
	return reserved, 1, cancel, true
}

func (*apiPermissionListenerCoordinator) ReleaseExclusive(string, uint64, context.CancelFunc) {}
func (*apiPermissionListenerCoordinator) ResetAutoResumeCounter(string)                       {}
func (c *apiPermissionListenerCoordinator) IsSessionBusy(string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.busy
}
func (*apiPermissionListenerCoordinator) Cancel(string) {}

func (c *apiPermissionListenerCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, _ string, _ uint64, _ context.CancelFunc, onHandoff func(), _, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.mu.Lock()
	c.busy = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.busy = false
		c.mu.Unlock()
		if c.cleaned != nil {
			c.cleaned <- struct{}{}
		}
	}()
	if onHandoff != nil {
		onHandoff()
	}
	c.started <- struct{}{}
	granted, err := c.app.Permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID: sessionID, ToolCallID: "integration-tool-call", ToolName: "edit", Action: "write", Description: "write integration file",
	})
	if err == nil && !granted {
		err = errors.New("tool permission denied")
	}
	c.results <- err
	return nil, err
}

type apiPermissionListenerTimer struct {
	ch chan time.Time
}

func (t *apiPermissionListenerTimer) C() <-chan time.Time { return t.ch }
func (*apiPermissionListenerTimer) Stop() bool            { return true }

func apiPermissionListenerRequest(t *testing.T, client *http.Client, baseURL, method, target, token, body string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, baseURL+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read/close response: %v/%v", err, closeErr)
	}
	return resp.StatusCode, data
}

func awaitAPIPermissionListenerSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal(message)
	}
}

func TestAPIPermissionsProductionListenerDecisions(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	var logs apiPermissionLogBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := &apiPermissionListenerCoordinator{app: app, started: make(chan struct{}, 1), results: make(chan error, 1), cleaned: make(chan struct{}, 1)}
	app.AgentCoordinator = coordinator
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("permission-listener", []string{"run", "read"}, "ask", nil)
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
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	events := make(chan apiPermissionEvent, 4)
	api.broker.SetEmitter(func(event apiPermissionEvent) { events <- event })
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}
	for _, decision := range []string{"allow", "deny"} {
		t.Run(decision, func(t *testing.T) {
			session, err := app.Sessions.Create(t.Context(), "permission listener "+decision)
			if err != nil {
				t.Fatal(err)
			}
			status, body := apiPermissionListenerRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+session.ID+"/runs", token, `{"prompt":"request permission"}`)
			if status != http.StatusAccepted {
				t.Fatalf("create run = %d %s", status, body)
			}
			awaitAPIPermissionListenerSignal(t, coordinator.started, "coordinator did not start")
			var event apiPermissionEvent
			select {
			case event = <-events:
			case <-time.After(10 * time.Second):
				t.Fatal("permission request was not published")
			}
			if event.Type != "permission_request" || event.Request.SessionID != session.ID {
				t.Fatalf("event = %+v", event)
			}
			status, body = apiPermissionListenerRequest(t, client, baseURL, http.MethodGet, "/api/v1/permissions", token, "")
			var pending []APIPermissionRequest
			if status != http.StatusOK || json.Unmarshal(body, &pending) != nil || len(pending) != 1 || pending[0].ID != event.Request.ID {
				t.Fatalf("pending list = %d %s", status, body)
			}
			status, body = apiPermissionListenerRequest(t, client, baseURL, http.MethodPost, "/api/v1/permissions/"+event.Request.ID, token, `{"decision":"`+decision+`"}`)
			if status != http.StatusOK {
				t.Fatalf("answer = %d %s", status, body)
			}
			select {
			case gotErr := <-coordinator.results:
				awaitAPIPermissionListenerSignal(t, coordinator.cleaned, "coordinator cleanup did not finish")
				if (decision == "allow") != (gotErr == nil) {
					t.Fatalf("tool result = %v for %s", gotErr, decision)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("tool result was not observed")
			}
		})
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), token[strings.IndexByte(token, '.')+1:]) {
		t.Fatal("plaintext token appeared in captured logs")
	}
}

func TestAPIPermissionsProductionListenerTimeoutDenies(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := &apiPermissionListenerCoordinator{app: app, started: make(chan struct{}, 1), results: make(chan error, 1), cleaned: make(chan struct{}, 1)}
	app.AgentCoordinator = coordinator
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("permission-timeout", []string{"run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := app.Sessions.Create(t.Context(), "permission timeout")
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
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	timer := &apiPermissionListenerTimer{ch: make(chan time.Time, 1)}
	api.broker.newTimer = func(time.Duration) apiPermissionTimer { return timer }
	api.broker.now = func() time.Time { return time.Unix(1, 0) }
	events := make(chan apiPermissionEvent, 1)
	api.broker.SetEmitter(func(event apiPermissionEvent) { events <- event })
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}
	status, body := apiPermissionListenerRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+session.ID+"/runs", token, `{"prompt":"timeout permission"}`)
	if status != http.StatusAccepted {
		t.Fatalf("create run = %d %s", status, body)
	}
	awaitAPIPermissionListenerSignal(t, coordinator.started, "coordinator did not start")
	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("permission request was not emitted")
	}
	timer.ch <- time.Now()
	select {
	case gotErr := <-coordinator.results:
		if gotErr == nil {
			t.Fatal("timed-out tool call was granted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed-out tool result was not observed")
	}
}

func TestAPIPermissionsProductionListenerReadonlyAndAutoDenied(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := &apiPermissionListenerCoordinator{app: app, started: make(chan struct{}, 1), results: make(chan error, 1), cleaned: make(chan struct{}, 1)}
	app.AgentCoordinator = coordinator
	store := config.GlobalAPITokenStore()
	readonlyToken, _, err := store.CreateAPIToken("readonly", []string{"run"}, "readonly", nil)
	if err != nil {
		t.Fatal(err)
	}
	autoToken, _, err := store.CreateAPIToken("auto", []string{"run"}, "auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := app.Sessions.Create(t.Context(), "readonly permission")
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
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}
	status, body := apiPermissionListenerRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+session.ID+"/runs", readonlyToken, `{"prompt":"readonly write"}`)
	if status != http.StatusAccepted {
		t.Fatalf("readonly run = %d %s", status, body)
	}
	awaitAPIPermissionListenerSignal(t, coordinator.started, "readonly coordinator did not start")
	select {
	case gotErr := <-coordinator.results:
		awaitAPIPermissionListenerSignal(t, coordinator.cleaned, "readonly coordinator cleanup did not finish")
		if gotErr == nil || gotErr.Error() != "tool permission denied" {
			t.Fatalf("readonly write result = %v", gotErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readonly write did not finish")
	}
	status, body = apiPermissionListenerRequest(t, client, baseURL, http.MethodGet, "/api/v1/permissions", readonlyToken, "")
	var pending []APIPermissionRequest
	if status != http.StatusOK || json.Unmarshal(body, &pending) != nil || len(pending) != 0 {
		t.Fatalf("readonly permission list = %d %s, want no prompt", status, body)
	}
	status, body = apiPermissionListenerRequest(t, client, baseURL, http.MethodPost, "/api/v1/sessions/"+session.ID+"/runs", autoToken, `{"prompt":"auto forbidden"}`)
	if status != http.StatusForbidden {
		t.Fatalf("auto run = %d %s, want forbidden", status, body)
	}
}
