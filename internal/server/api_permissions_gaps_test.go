package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

func sendPermissionEvent(service *apiPermissionTestService, req permission.PermissionRequest) {
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: req}
}

func TestAPIPermissionBrokerPendingFiltersByOwnerAndSession(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run-a", "sess-a", "token-a"); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register("run-b", "sess-b", "token-b"); err != nil {
		t.Fatal(err)
	}
	sendPermissionEvent(service, permission.PermissionRequest{ID: "pa", SessionID: "sess-a"})
	awaitPermission(t, service, broker, "token-a", "pa")
	sendPermissionEvent(service, permission.PermissionRequest{ID: "pb", SessionID: "sess-b"})
	awaitPermission(t, service, broker, "token-b", "pb")

	if got := broker.Pending("token-a", false, ""); len(got) != 1 || got[0].ID != "pa" {
		t.Fatalf("token-a sees %+v, want only pa", got)
	}
	if got := broker.Pending("token-c", false, ""); len(got) != 0 {
		t.Fatalf("foreign token sees %+v", got)
	}
	if got := broker.Pending("admin", true, ""); len(got) != 2 {
		t.Fatalf("admin sees %+v, want both", got)
	}
	if got := broker.Pending("admin", true, "sess-b"); len(got) != 1 || got[0].ID != "pb" {
		t.Fatalf("session filter returned %+v, want only pb", got)
	}
}

func TestAPIPermissionBrokerIgnoresUnregisteredSessions(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "sess", "token"); err != nil {
		t.Fatal(err)
	}
	sendPermissionEvent(service, permission.PermissionRequest{ID: "foreign", SessionID: "unregistered"})
	sendPermissionEvent(service, permission.PermissionRequest{ID: "mine", SessionID: "sess"})
	awaitPermission(t, service, broker, "token", "mine")
	if got := broker.Pending("admin", true, ""); len(got) != 1 {
		t.Fatalf("pending = %+v, want only the registered session request", got)
	}
}

func TestAPIPermissionBrokerRegisterRejectsSecondRunOnSession(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run-1", "sess", "token"); err != nil {
		t.Fatal(err)
	}
	if err := broker.Register("run-2", "sess", "token"); !errors.Is(err, ErrAPIPermissionUnknown) {
		t.Fatalf("second run on the same session: err = %v", err)
	}
	if err := broker.Register("run-1", "sess", "token"); err != nil {
		t.Fatalf("re-registering the same run: %v", err)
	}
}

func TestAPIPermissionBrokerStopDeniesOutstandingRequests(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer cancel()
	if err := broker.Register("run", "sess", "token"); err != nil {
		t.Fatal(err)
	}
	sendPermissionEvent(service, permission.PermissionRequest{ID: "p1", SessionID: "sess"})
	awaitPermission(t, service, broker, "token", "p1")
	broker.Stop()
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.denies != 1 || service.grants != 0 {
		t.Fatalf("after Stop: denies=%d grants=%d, want the pending request denied", service.denies, service.grants)
	}
}

func TestAPIPermissionBrokerTruncatesDescription(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "sess", "token"); err != nil {
		t.Fatal(err)
	}
	sendPermissionEvent(service, permission.PermissionRequest{ID: "p1", SessionID: "sess", Description: strings.Repeat("ж", 500)})
	item := awaitPermission(t, service, broker, "token", "p1")
	if got := len([]rune(item.Description)); got != permissionDescriptionLimit {
		t.Fatalf("description length = %d runes, want %d", got, permissionDescriptionLimit)
	}
}

func TestAPIPermissionBrokerBoundsDecidedTombstones(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "sess", "token"); err != nil {
		t.Fatal(err)
	}
	total := maxPendingPermissionsPerSession + 2
	idOf := func(i int) string { return "t" + string(rune('A'+i/26)) + string(rune('a'+i%26)) }
	for i := 0; i < total; i++ {
		id := idOf(i)
		sendPermissionEvent(service, permission.PermissionRequest{ID: id, SessionID: "sess"})
		awaitPermission(t, service, broker, "token", id)
		if err := broker.Answer(id, "token", "allow", false); err != nil {
			t.Fatalf("answer %s: %v", id, err)
		}
		select {
		case <-service.decisionSignal:
		case <-time.After(10 * time.Second):
			t.Fatal("decision was not applied")
		}
	}
	if err := broker.Answer(idOf(0), "token", "allow", false); !errors.Is(err, ErrAPIPermissionUnknown) {
		t.Fatalf("evicted tombstone: err = %v, want unknown", err)
	}
	if err := broker.Answer(idOf(total-1), "token", "allow", false); !errors.Is(err, ErrAPIPermissionDecided) {
		t.Fatalf("recent tombstone: err = %v, want already decided", err)
	}
}

func TestAPIPermissionBrokerDecidedTombstoneIsOwnerScoped(t *testing.T) {
	permissionTestEnv(t)
	service := newAPIPermissionTestService()
	broker, cancel := newTestPermissionBroker(t, service, "")
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "sess", "token-a"); err != nil {
		t.Fatal(err)
	}
	sendPermissionEvent(service, permission.PermissionRequest{ID: "p1", SessionID: "sess"})
	awaitPermission(t, service, broker, "token-a", "p1")
	if err := broker.Answer("p1", "token-a", "allow", false); err != nil {
		t.Fatal(err)
	}
	if err := broker.Answer("p1", "token-b", "allow", false); !errors.Is(err, ErrAPIPermissionUnknown) {
		t.Fatalf("foreign token on a decided request: err = %v, want unknown (no existence leak)", err)
	}
	if err := broker.Answer("p1", "token-a", "allow", false); !errors.Is(err, ErrAPIPermissionDecided) {
		t.Fatalf("owner repeat: err = %v, want already decided", err)
	}
	if err := broker.Answer("p1", "admin", "allow", true); !errors.Is(err, ErrAPIPermissionDecided) {
		t.Fatalf("admin repeat: err = %v, want already decided", err)
	}
}
