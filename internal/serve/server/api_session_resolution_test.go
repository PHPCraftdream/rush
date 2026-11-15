package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
)

func TestSessionResolutionTableExhaustive(t *testing.T) {
	for command := range commandScopes {
		if _, _, ok := SessionResolutionFor(command); !ok {
			t.Errorf("command %q has no session resolution", command)
		}
	}
	for _, command := range apiauth.SessionResolutionCommands() {
		if _, ok := commandScopes[command]; !ok {
			t.Errorf("resolution for unknown command %q", command)
		}
	}
}

func TestRESTSessionResolutionTableExhaustive(t *testing.T) {
	for _, route := range APIRoutes() {
		if !RouteClassIsKnown(route.OperationID) {
			t.Errorf("route %s %s (%s) has no resolution", route.Method, route.Pattern, route.OperationID)
		}
	}
}

func TestResolvePinnedCommandVisibilityBeforeClass(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		body string
		want int
	}{
		{"foreign forbidden operation hidden", CmdDeleteSession, `{"sessionID":"foreign"}`, http.StatusNotFound},
		{"same-session forbidden operation denied", CmdDeleteSession, `{"sessionID":"pinned"}`, http.StatusForbidden},
		{"foreign readable operation hidden", CmdLoadMessages, `{"sessionID":"foreign"}`, http.StatusNotFound},
		{"pinned read allowed", CmdLoadMessages, `{"sessionID":"pinned"}`, 0},
		{"missing target hidden", CmdLoadMessages, `{}`, http.StatusNotFound},
		{"global peak pause denied", CmdPauseProviderPeakHours, `{"id":"provider","sessionID":"pinned"}`, http.StatusForbidden},
		{"global peak resume denied", CmdResumeProviderPeakHours, `{"id":"provider","sessionID":"pinned"}`, http.StatusForbidden},
		{"global peak status denied", CmdGetPeakHoursStatus, `{"id":"provider","sessionID":"pinned"}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePinnedCommand("pinned", tc.cmd, json.RawMessage(tc.body)); got != tc.want {
				t.Fatalf("status=%d want %d", got, tc.want)
			}
		})
	}
}

func TestPinnedRESTListSessionsIsFilteredNotDenied(t *testing.T) {
	route := APIRoute{OperationID: "listSessions"}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sessions", nil)
	if got := ResolvePinnedRoute("pinned-session", route.OperationID, r.PathValue("id")); got != 0 {
		t.Fatalf("status=%d want 0 (the handler filters)", got)
	}
}

func TestResolvePinnedRouteVisibilityBeforeClass(t *testing.T) {
	cases := []struct {
		operation, id string
		want          int
	}{
		{"getSession", "other", http.StatusNotFound},
		{"deleteSession", "other", http.StatusNotFound},
		{"deleteSession", "pinned", http.StatusForbidden},
		{"getSession", "pinned", 0},
		{"listSessions", "", 0},
		{"getRun", "", 0},
		{"cancelRun", "", 0},
		{"listPermissions", "", 0},
		{"answerPermission", "", 0},
		{"createSession", "", http.StatusForbidden},
		{"unmapped", "pinned", http.StatusForbidden},
	}
	for _, tc := range cases {
		route := APIRoute{OperationID: tc.operation}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.SetPathValue("id", tc.id)
		if got := ResolvePinnedRoute("pinned", route.OperationID, r.PathValue("id")); got != tc.want {
			t.Errorf("%s/%s: got %d want %d", tc.operation, tc.id, got, tc.want)
		}
	}
}

func TestSessionResolutionCommandSetMatchesScopeCommandSet(t *testing.T) {
	commands := apiauth.SessionResolutionCommands()
	if len(commands) != len(commandScopes) {
		t.Fatalf("resolution commands=%d scopes=%d", len(commands), len(commandScopes))
	}
	for command := range commandScopes {
		found := false
		for _, item := range commands {
			if item == command {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q", command)
		}
	}
	if len(commands) != len(commandScopes) {
		t.Fatal("command cardinality mismatch")
	}
}
