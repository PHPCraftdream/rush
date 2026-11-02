package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionResolutionTableExhaustive(t *testing.T) {
	for command := range commandScopes {
		if _, ok := sessionResolutionTable[command]; !ok {
			t.Errorf("command %q has no session resolution", command)
		}
	}
	for command := range sessionResolutionTable {
		if _, ok := commandScopes[command]; !ok {
			t.Errorf("resolution for unknown command %q", command)
		}
	}
}

func TestRESTSessionResolutionTableExhaustive(t *testing.T) {
	for _, route := range APIRoutes() {
		if !routeClassIsKnown(route) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePinnedCommand("pinned", tc.cmd, json.RawMessage(tc.body)); got != tc.want {
				t.Fatalf("status=%d want %d", got, tc.want)
			}
		})
	}
}

func TestPinnedRESTListSessionsIsFilteredNotDenied(t *testing.T) {
	route := APIRoute{OperationID: "listSessions"}
	r := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	if got := resolvePinnedRoute("pinned-session", route, r); got != 0 {
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
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("id", tc.id)
		if got := resolvePinnedRoute("pinned", route, r); got != tc.want {
			t.Errorf("%s/%s: got %d want %d", tc.operation, tc.id, got, tc.want)
		}
	}
}

func TestSessionResolutionCommandSetMatchesScopeCommandSet(t *testing.T) {
	commands := sessionResolutionCommands()
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
