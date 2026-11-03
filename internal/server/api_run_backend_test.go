package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type apiRunBackendGateManager struct {
	permission.Service
	mu          sync.Mutex
	calls       []string
	denied      map[string]string
	armEvent    string
	events      *apiRunBackendGateEvents
	cancelOnArm context.CancelFunc
	onRelease   func(sessionID, ownerToken string)
}

type apiRunBackendGateEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *apiRunBackendGateEvents) add(event string) {
	e.mu.Lock()
	e.values = append(e.values, event)
	e.mu.Unlock()
}

func (e *apiRunBackendGateEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

func (m *apiRunBackendGateManager) DenySession(sessionID, ownerToken string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "arm")
	if m.events != nil {
		m.events.add("arm")
	}
	m.armEvent = sessionID + ":" + ownerToken
	if m.denied == nil {
		m.denied = make(map[string]string)
	}
	m.denied[sessionID] = ownerToken
	if m.cancelOnArm != nil {
		m.cancelOnArm()
	}
}

func (m *apiRunBackendGateManager) ReleaseSessionDeny(sessionID, ownerToken string) {
	if m.onRelease != nil {
		m.onRelease(sessionID, ownerToken)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "release")
	if m.events != nil {
		m.events.add("release")
	}
	if m.denied[sessionID] == ownerToken {
		delete(m.denied, sessionID)
	}
}

func (m *apiRunBackendGateManager) SessionDenied(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.denied[sessionID] != ""
}

func (m *apiRunBackendGateManager) snapshot() ([]string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := append([]string(nil), m.calls...)
	owner := m.denied["api-gate-session"]
	return calls, owner, m.armEvent
}

type apiRunBackendGateCoordinator struct {
	agent.Coordinator
	mu         sync.Mutex
	calls      []string
	reserve    int
	events     *apiRunBackendGateEvents
	runStarted chan struct{}
	runReturn  chan error
	cancelled  []string
}

func (c *apiRunBackendGateCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (context.Context, uint64, context.CancelFunc, bool) {
	c.mu.Lock()
	c.reserve++
	c.mu.Unlock()
	cancelCtx, cancel := context.WithCancel(ctx)
	return cancelCtx, 1, cancel, true
}

func (c *apiRunBackendGateCoordinator) ReleaseExclusive(string, uint64, context.CancelFunc) {}

func (c *apiRunBackendGateCoordinator) ResetAutoResumeCounter(string) {}

func (c *apiRunBackendGateCoordinator) Cancel(sessionID string) {
	c.mu.Lock()
	c.cancelled = append(c.cancelled, sessionID)
	c.mu.Unlock()
}

func (c *apiRunBackendGateCoordinator) RunWithReservedOwnership(ctx context.Context, _, _ string, _ uint64, _ context.CancelFunc, onHandoff func(), _, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, "start")
	c.mu.Unlock()
	if c.events != nil {
		c.events.add("start")
	}
	if onHandoff != nil {
		onHandoff()
	}
	if c.runStarted != nil {
		c.runStarted <- struct{}{}
	}
	if c.runReturn == nil {
		return nil, nil
	}
	select {
	case err := <-c.runReturn:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *apiRunBackendGateCoordinator) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func newAPIRunBackendGateApp(t *testing.T, manager *apiRunBackendGateManager, runReturn chan error) (*appPkg.App, *apiRunBackendGateCoordinator) {
	t.Helper()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	manager.Service = app.Permissions
	events := &apiRunBackendGateEvents{}
	manager.events = events
	coordinator := &apiRunBackendGateCoordinator{
		Coordinator: nil,
		events:      events,
		runStarted:  make(chan struct{}, 1),
		runReturn:   runReturn,
	}
	app.AgentCoordinator = coordinator
	app.Permissions = manager
	return app, coordinator
}

func awaitAPIRunGateEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for coordinator start")
	}
}

func awaitAPIRunGateOutcome(t *testing.T, done <-chan RunOutcome) RunOutcome {
	t.Helper()
	select {
	case outcome, ok := <-done:
		if !ok {
			t.Fatal("run completed without an outcome")
		}
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run completion")
		return RunOutcome{}
	}
}

func TestAPIRunBackendCancelStopsCoordinatorForTargetedRun(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error)
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	backend := newAPIRunBackend(app)
	done, err := backend.Start(t.Context(), "run-cancel-target", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.True(t, backend.Cancel("api-gate-session", "run-cancel-target"))
	require.Equal(t, []string{"api-gate-session"}, coordinator.cancelled)
	require.Equal(t, APIRunCancelled, awaitAPIRunGateOutcome(t, done).State)
	require.False(t, backend.Cancel("api-gate-session", "run-cancel-target"))
	require.Equal(t, []string{"api-gate-session"}, coordinator.cancelled)
}

func TestAPIRunBackendCancelRejectsDifferentRunWithoutStoppingCoordinator(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error)
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	backend := newAPIRunBackend(app)
	done, err := backend.Start(t.Context(), "run-cancel-target", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.False(t, backend.Cancel("api-gate-session", "different-run"))
	require.Empty(t, coordinator.cancelled)
	require.True(t, backend.Cancel("api-gate-session", "run-cancel-target"))
	require.Equal(t, APIRunCancelled, awaitAPIRunGateOutcome(t, done).State)
}

func TestAPIRunBackendCancelOldRunDoesNotStopLaterRun(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error, 1)
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	backend := newAPIRunBackend(app)
	firstDone, err := backend.Start(t.Context(), "run-first", "api-gate-session", "first prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	runReturn <- nil
	require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, firstDone).State)

	secondDone, err := backend.Start(t.Context(), "run-second", "api-gate-session", "second prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.False(t, backend.Cancel("api-gate-session", "run-first"))
	require.Empty(t, coordinator.cancelled)
	require.True(t, backend.Cancel("api-gate-session", "run-second"))
	require.Equal(t, []string{"api-gate-session"}, coordinator.cancelled)
	require.Equal(t, APIRunCancelled, awaitAPIRunGateOutcome(t, secondDone).State)
}

func TestAPIRunBackendGateArmsBeforeCoordinatorAndReleasesOnSuccess(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	app, coordinator := newAPIRunBackendGateApp(t, manager, nil)
	backend := newAPIRunBackend(app)
	done, err := backend.Start(t.Context(), "run-owner-1", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.Equal(t, APIRunSucceeded, awaitAPIRunGateOutcome(t, done).State)
	_, owner, armed := manager.snapshot()
	require.Equal(t, []string{"arm", "start", "release"}, manager.events.snapshot())
	require.Equal(t, "api-gate-session:run-owner-1", armed)
	require.Empty(t, owner)
	require.Equal(t, 1, coordinator.startCount())
}

func TestAPIRunBackendGateReleasesAfterCoordinatorFailure(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error, 1)
	runReturn <- errors.New("coordinator failed")
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	done, err := newAPIRunBackend(app).Start(t.Context(), "run-owner-2", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.Equal(t, APIRunFailed, awaitAPIRunGateOutcome(t, done).State)
	_, owner, _ := manager.snapshot()
	require.Equal(t, []string{"arm", "start", "release"}, manager.events.snapshot())
	require.Empty(t, owner)
}

func TestAPIRunBackendGateReleasesAfterCancellation(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error)
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	ctx, cancel := context.WithCancel(t.Context())
	done, err := newAPIRunBackend(app).Start(ctx, "run-owner-3", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	cancel()
	require.Equal(t, APIRunCancelled, awaitAPIRunGateOutcome(t, done).State)
	_, owner, _ := manager.snapshot()
	require.Equal(t, []string{"arm", "start", "release"}, manager.events.snapshot())
	require.Empty(t, owner)
}

func TestAPIRunBackendGateReleasesAfterStartError(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	runReturn := make(chan error, 1)
	runReturn <- errors.New("start operation failed")
	app, coordinator := newAPIRunBackendGateApp(t, manager, runReturn)
	done, err := newAPIRunBackend(app).Start(t.Context(), "run-owner-4", "api-gate-session", "prompt", "", "")
	require.NoError(t, err)
	awaitAPIRunGateEvent(t, coordinator.runStarted)
	require.Equal(t, APIRunFailed, awaitAPIRunGateOutcome(t, done).State)
	_, owner, _ := manager.snapshot()
	require.Equal(t, []string{"arm", "start", "release"}, manager.events.snapshot())
	require.Empty(t, owner)
}

func TestAPIRunBackendGateReleasesOnEarlyCancelledRun(t *testing.T) {
	manager := &apiRunBackendGateManager{}
	app, coordinator := newAPIRunBackendGateApp(t, manager, nil)
	ctx, cancel := context.WithCancel(t.Context())
	manager.cancelOnArm = cancel
	_, err := newAPIRunBackend(app).Start(ctx, "run-owner-early-cancel", "api-gate-session", "prompt", "", "")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, coordinator.startCount())
	_, owner, _ := manager.snapshot()
	require.Empty(t, owner)
	require.Equal(t, []string{"arm", "release"}, manager.events.snapshot())
}

func TestAPIRunBackendGateUnavailableRefusesBeforeCoordinatorStarts(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	coordinator := &apiRunBackendGateCoordinator{Coordinator: nil, runStarted: make(chan struct{}, 1)}
	app.AgentCoordinator = coordinator
	app.Permissions = nil
	store := &apiRunHandlerStore{item: session.Session{ID: "api-gate-session"}}
	result := apiRunRequest(t, apiRunHandler(store, newAPIRunBackend(app)), http.MethodPost, "/api/v1/sessions/api-gate-session/runs", `{"prompt":"prompt"}`, Principal{TokenID: "token", Scopes: []string{"run"}}, nil)
	require.Equal(t, http.StatusServiceUnavailable, result.status)
	require.Equal(t, "application/problem+json", result.header.Get("Content-Type"))
	require.Contains(t, string(result.body), `"title":"Service Unavailable"`)
	require.Contains(t, string(result.body), `"detail":"permission gate unavailable"`)
	require.NotContains(t, string(result.body), "Internal Server Error")
	require.Zero(t, coordinator.startCount())
	coordinator.mu.Lock()
	reserveCount := coordinator.reserve
	coordinator.mu.Unlock()
	require.Zero(t, reserveCount)
}

func TestAPIRunBackendGateSourceDoesNotUseLegacyExecutionPaths(t *testing.T) {
	files, err := filepath.Glob("api_run_backend*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(file)
		require.NoError(t, readErr)
		source := string(data)
		require.NotContains(t, source, "AutoApproveSession", file)
		require.NotContains(t, source, "ExecuteRun", file)
	}
}

func TestAPIRunBackendGateBusySessionRemainsConflict(t *testing.T) {
	manager := &apiRunBackendGateManager{denied: map[string]string{"api-gate-session": "another-run"}}
	app, coordinator := newAPIRunBackendGateApp(t, manager, nil)
	backend := newAPIRunBackend(app)
	_, err := backend.Start(t.Context(), "run-owner-5", "api-gate-session", "prompt", "", "")
	require.ErrorIs(t, err, ErrAPIRunBackendBusy)
	require.Zero(t, coordinator.startCount())
	calls, owner, _ := manager.snapshot()
	require.Empty(t, calls)
	require.Equal(t, "another-run", owner)
}

// CancelAll keeps App shutdown safe when no real coordinator is configured (CI has no providers).
func (c *apiRunBackendGateCoordinator) CancelAll() bool { return false }

func (c *apiRunBackendGateCoordinator) IsSessionBusy(string) bool { return false }
