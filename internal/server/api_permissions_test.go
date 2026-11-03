package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

type apiPermissionTestService struct {
	permission.Service
	events                     chan pubsub.Event[permission.PermissionRequest]
	notifications              chan apiPermissionEvent
	mu                         sync.Mutex
	grants, denies, persistent int
	decisionSignal             chan struct{}
}

func newAPIPermissionTestService() *apiPermissionTestService {
	return &apiPermissionTestService{events: make(chan pubsub.Event[permission.PermissionRequest], 256), notifications: make(chan apiPermissionEvent, 256), decisionSignal: make(chan struct{}, 256)}
}

func (s *apiPermissionTestService) Subscribe(context.Context) <-chan pubsub.Event[permission.PermissionRequest] {
	return s.events
}

func (s *apiPermissionTestService) Grant(permission.PermissionRequest) {
	s.mu.Lock()
	s.grants++
	s.mu.Unlock()
	s.decisionSignal <- struct{}{}
}

func (s *apiPermissionTestService) Deny(permission.PermissionRequest) {
	s.mu.Lock()
	s.denies++
	s.mu.Unlock()
	s.decisionSignal <- struct{}{}
}

func (s *apiPermissionTestService) GrantPersistent(permission.PermissionRequest) {
	s.mu.Lock()
	s.persistent++
	s.mu.Unlock()
}

type apiPermissionTestTimer struct {
	ch      chan time.Time
	stopped chan struct{}
	once    sync.Once
}

func newAPIPermissionTestTimer() *apiPermissionTestTimer {
	return &apiPermissionTestTimer{ch: make(chan time.Time, 1), stopped: make(chan struct{})}
}
func (t *apiPermissionTestTimer) C() <-chan time.Time { return t.ch }
func (t *apiPermissionTestTimer) Stop() bool {
	stopped := false
	t.once.Do(func() { close(t.stopped); stopped = true })
	return stopped
}
func (t *apiPermissionTestTimer) fire() { t.ch <- time.Unix(1, 0) }

func permissionTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(t.TempDir(), "config"))
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
}

func newTestPermissionBroker(t *testing.T, service *apiPermissionTestService, workspace string) (*APIPermissionBroker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	broker := NewAPIPermissionBroker(service, workspace, func(event apiPermissionEvent) { service.notifications <- event }, time.Hour)
	broker.newTimer = func(time.Duration) apiPermissionTimer { return newAPIPermissionTestTimer() }
	if err := broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return broker, cancel
}

func awaitPermission(t *testing.T, service *apiPermissionTestService, broker *APIPermissionBroker, token, id string) APIPermissionRequest {
	t.Helper()
	select {
	case event := <-service.notifications:
		if event.Request.ID != id {
			t.Fatalf("notification id = %q, want %q", event.Request.ID, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("permission %q did not become pending", id)
	}
	for _, item := range broker.Pending(token, false, "") {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("permission %q notification preceded pending insertion", id)
	return APIPermissionRequest{}
}

func TestAPIPermissionBrokerPendingBoundsAndRedactsPaths(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	ctx, cancel := context.WithCancel(context.Background())
	broker := NewAPIPermissionBroker(service, `C:\workspace`, func(event apiPermissionEvent) { service.notifications <- event }, time.Hour)
	broker.newTimer = func(time.Duration) apiPermissionTimer { return newAPIPermissionTestTimer() }
	if err := broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "session", "token"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: string(rune('a' + i)), SessionID: "session", Path: `C:\workspace\secret\file.txt`}}
	}
	for i := 0; i < 101; i++ {
		select {
		case <-service.notifications:
		case <-time.After(10 * time.Second):
			t.Fatal("broker did not process all requests")
		}
	}
	list := broker.Pending("token", false, "")
	if len(list) != 100 {
		t.Fatalf("pending count = %d", len(list))
	}
	for _, item := range list {
		if filepath.IsAbs(item.Path) || item.Path != "secret/file.txt" {
			t.Fatalf("path was not redacted: %q", item.Path)
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.denies != 1 {
		t.Fatalf("overflow denials = %d, want 1", service.denies)
	}
}

func TestAPIPermissionBrokerTimeoutDeniesExactlyOnce(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "session", "token"); err != nil {
		t.Fatal(err)
	}
	timer := newAPIPermissionTestTimer()
	broker.newTimer = func(time.Duration) apiPermissionTimer { return timer }
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "timeout", SessionID: "session"}}
	awaitPermission(t, service, broker, "token", "timeout")
	timer.fire()
	select {
	case <-service.decisionSignal:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout did not deny")
	}
	broker.Stop()
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.denies != 1 {
		t.Fatalf("denies = %d", service.denies)
	}
}

func TestAPIPermissionBrokerAnswerOwnershipAndExactlyOneDecision(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "session", "token-a"); err != nil {
		t.Fatal(err)
	}
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "p", SessionID: "session"}}
	awaitPermission(t, service, broker, "token-a", "p")
	if err := broker.Answer("p", "token-b", "allow", false); err != ErrAPIPermissionUnknown {
		t.Fatalf("foreign answer error = %v", err)
	}
	if err := broker.Answer("p", "", "allow_session", true); err != ErrAPIPermissionDecision {
		t.Fatalf("allow_session error = %v", err)
	}
	if err := broker.Answer("p", "token-a", "allow", false); err != nil {
		t.Fatal(err)
	}
	if err := broker.Answer("p", "", "deny", true); err != ErrAPIPermissionDecided {
		t.Fatalf("duplicate answer error = %v", err)
	}
	broker.Release("run")
	if err := broker.Answer("p", "token-a", "deny", false); err != ErrAPIPermissionUnknown {
		t.Fatalf("answer after release error = %v", err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.grants != 1 || service.denies != 0 || service.persistent != 0 {
		t.Fatalf("decisions: grant=%d deny=%d persistent=%d", service.grants, service.denies, service.persistent)
	}
}

func TestAPIPermissionBrokerReleaseAndStopJoin(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	started := make(chan struct{})
	unblock := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	broker := NewAPIPermissionBroker(service, "", func(apiPermissionEvent) {
		close(started)
		<-unblock
	}, time.Hour)
	broker.newTimer = func(time.Duration) apiPermissionTimer { return newAPIPermissionTestTimer() }
	if err := broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register("run", "session", "token"); err != nil {
		t.Fatal(err)
	}
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "active", SessionID: "session"}}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("broker did not begin emitting")
	}
	broker.Release("run")
	if len(broker.Pending("token", false, "")) != 0 {
		t.Fatal("release retained requests")
	}
	cancel()
	stopStarted := make(chan struct{})
	broker.waitStarted = func() { close(stopStarted) }
	joined := make(chan struct{})
	go func() { broker.Stop(); close(joined) }()
	select {
	case <-stopStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not begin joining")
	}
	select {
	case <-joined:
		t.Fatal("Stop returned before the active emitter joined")
	default:
	}
	close(unblock)
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not join")
	}
}

func TestAPIPermissionBrokerClosedSubscriptionFailsClosed(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	close(service.events)
	select {
	case <-broker.done:
	case <-time.After(10 * time.Second):
		t.Fatal("broker did not observe closed subscription")
	}
	if err := broker.Register("run", "session", "token"); err == nil {
		t.Fatal("registration succeeded after subscription closed")
	}
}

func TestAPIPermissionBrokerAnswerTimeoutRace(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "session", "token"); err != nil {
		t.Fatal(err)
	}
	timer := newAPIPermissionTestTimer()
	broker.newTimer = func(time.Duration) apiPermissionTimer { return timer }
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "race", SessionID: "session"}}
	awaitPermission(t, service, broker, "token", "race")
	start := make(chan struct{})
	result := make(chan error, 1)
	go func() { <-start; result <- broker.Answer("race", "token", "allow", false) }()
	go func() { <-start; timer.fire() }()
	close(start)
	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("answer race did not complete")
	}
	select {
	case <-service.decisionSignal:
	case <-time.After(10 * time.Second):
		t.Fatal("answer/timeout race made no decision")
	}
	broker.Stop()
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.grants+service.denies != 1 {
		t.Fatalf("decisions: grant=%d deny=%d", service.grants, service.denies)
	}
}

func TestAPIPermissionBrokerWorkspacePathFallsBackToBase(t *testing.T) {
	if got := redactPermissionPath(`C:\workspace\secret.txt`, `C:\workspace`); got != "secret.txt" {
		t.Fatalf("Windows relative path = %q", got)
	}
	if got := redactPermissionPath(`/outside/private.txt`, ""); got != "private.txt" {
		t.Fatalf("redacted path = %q", got)
	}
}
