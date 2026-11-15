package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func batchBoundBinding() config.APITokenBinding {
	return config.APITokenBinding{Kind: "batch", Batch: &config.APITokenBatchBinding{MaxRoots: 2, MaxParallel: 2}}
}

// createConversation makes a conversational session through the unbound token.
func (f *batchFixture) createConversation(t *testing.T, unbound string) string {
	t.Helper()
	resp := f.do(t, unbound, http.MethodPost, "/sessions", map[string]any{"title": "chat"})
	require.Equal(t, http.StatusCreated, resp.Status, string(resp.Body))
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &created))
	require.NotEmpty(t, created.ID)
	return created.ID
}

// requireBatchOnlyDenied asserts the REST conversation routes are refused with
// the batch-only problem, and that nothing changed.
func (f *batchFixture) requireBatchOnlyDenied(t *testing.T, token, unbound, sid string) {
	t.Helper()
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"create session", http.MethodPost, "/sessions", map[string]any{"title": "x"}},
		{"run", http.MethodPost, "/sessions/" + sid + "/runs", map[string]any{"prompt": "x"}},
		{"inject", http.MethodPost, "/sessions/" + sid + "/inject", map[string]any{"message": "x"}},
		{"fork", http.MethodPost, "/sessions/" + sid + "/fork", map[string]any{"title": "x"}},
		{"delete", http.MethodDelete, "/sessions/" + sid, nil},
	}
	for _, tc := range cases {
		resp := f.do(t, token, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, resp.Status, "%s: %s", tc.name, resp.Body)
		require.Contains(t, string(resp.Body), "batch-only", tc.name)
	}
	require.Equal(t, http.StatusOK, f.do(t, unbound, http.MethodGet, "/sessions/"+sid, nil).Status, "the denied delete must not have removed the session")
}

// requireBatchOnlyAllowed asserts reads and the batch routes still work.
func (f *batchFixture) requireBatchOnlyAllowed(t *testing.T, token, sid string) {
	t.Helper()
	for _, path := range []string{"/health", "/version", "/models", "/sessions", "/sessions/" + sid, "/sessions/" + sid + "/messages", "/permissions"} {
		require.Equal(t, http.StatusOK, f.do(t, token, http.MethodGet, path, nil).Status, path)
	}
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodGet, "/runs/no-such-run", nil).Status, "an unknown run is not found, not forbidden")
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "hello"}).accepted(t)
	f.waitBatchDone(t, token, accepted.RootID)
}

func TestBatchOnlyTokenCannotUseConversationRoutes(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	bound := f.token(t, "bound", []string{"read", "run"}, "ask", batchBoundBinding())
	unbound := f.token(t, "unbound", []string{"read", "run"}, "ask", config.APITokenBinding{})
	sid := f.createConversation(t, unbound)

	f.requireBatchOnlyDenied(t, bound, unbound, sid)
	f.requireBatchOnlyAllowed(t, bound, sid)

	// Control: the same routes work for the unbound token, so the refusal is the binding's.
	require.Equal(t, http.StatusCreated, f.do(t, unbound, http.MethodPost, "/sessions", map[string]any{"title": "again"}).Status)
}

func TestBatchRouteListenerConfinesEveryToken(t *testing.T) {
	f := newBatchFixtureWith(t, batchTextProvider("ok"), func(p *ListenerPolicy) { p.Route = routeBatch })
	unbound := f.token(t, "unbound", []string{"read", "run"}, "ask", config.APITokenBinding{})
	// Creating the conversation is itself a refused route on this listener, so
	// the session comes from the App directly.
	created, err := f.app.Sessions.Create(t.Context(), "chat")
	require.NoError(t, err)

	f.requireBatchOnlyDenied(t, unbound, unbound, created.ID)
	f.requireBatchOnlyAllowed(t, unbound, created.ID)
}

func TestBatchOnlyWebSocketCommands(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	bound := f.token(t, "bound", []string{"read", "run"}, "ask", batchBoundBinding())
	unbound := f.token(t, "unbound", []string{"read", "run"}, "ask", config.APITokenBinding{})
	endpoint := "ws" + strings.TrimPrefix(f.base, "http")

	scope := &scopeFixture{endpoint: endpoint, tokens: map[string]string{"bound": bound, "unbound": unbound}}
	boundConn := scope.dial(t, "bound")
	for _, command := range []string{CmdSendMessage, CmdInjectMessage, CmdCreateSession, CmdForkSession, CmdDeleteSession, CmdInterruptAndSend} {
		reply, _ := exchange(t, boundConn, WSMessage{ID: "c-" + command, Type: command, Payload: []byte(`{"sessionID":"s","content":"x"}`)})
		require.Equal(t, EventError, reply.Type, command)
		require.Equal(t, "forbidden: batch-only", reply.Error, command)
	}
	reply, _ := exchange(t, boundConn, WSMessage{ID: "list", Type: CmdListSessions})
	require.Equal(t, EventSessionsList, reply.Type, "reads stay available")

	// Control: the unbound token is not refused by the batch-only rule.
	unboundConn := scope.dial(t, "unbound")
	sendCommand(t, unboundConn, WSMessage{ID: "create", Type: CmdCreateSession, Payload: []byte(`{"title":"x"}`)})
	awaitType(t, unboundConn, EventSessionCreated)
}

// The classification must name every route: a new route forces a decision here.
func TestBatchOnlyRouteClassificationIsExplicit(t *testing.T) {
	allowed := map[string]bool{
		"getHealth": true, "getVersion": true, "getOpenAPI": true, "listModels": true,
		"listSessions": true, "getSession": true, "listMessages": true, "streamSessionEvents": true,
		"getRun": true, "cancelRun": true, "listPermissions": true, "answerPermission": true,
		"createBatch": true, "createTask": true, "getBatch": true, "getBatchNode": true,
		"stopBatch": true, "stopBatchNode": true, "answerBatchQuestion": true,
	}
	denied := map[string]bool{"createRun": true, "injectMessage": true, "createSession": true, "forkSession": true, "deleteSession": true}
	var seen []string
	for _, route := range APIRoutes() {
		seen = append(seen, route.OperationID)
		status := resolveBatchOnlyRoute(route)
		switch {
		case allowed[route.OperationID]:
			require.Zero(t, status, route.OperationID)
		case denied[route.OperationID]:
			require.Equal(t, http.StatusForbidden, status, route.OperationID)
		default:
			require.Failf(t, "unclassified route", "%s has no batch-only decision", route.OperationID)
		}
	}
	sort.Strings(seen)
	for id := range allowed {
		require.Contains(t, seen, id)
	}
	for id := range denied {
		require.Contains(t, seen, id)
	}
	require.Equal(t, http.StatusForbidden, resolveBatchOnlyRoute(APIRoute{OperationID: "somethingNew"}), "an unknown route fails closed")
}

func TestBatchOnlyCommandClassificationIsExplicit(t *testing.T) {
	allowed := map[string]bool{
		CmdListSessions: true, CmdLoadMessages: true, CmdGetSessionLiveWork: true, CmdGetSessionWakeSchedules: true,
		CmdGetSessionBatchWork: true, CmdBatchStatus: true, CmdBatchOutput: true, CmdCancelAgent: true,
	}
	for _, command := range sessionResolutionCommands() {
		status := resolveBatchOnlyCommand(command)
		if allowed[command] {
			require.Zero(t, status, command)
		} else {
			require.Equal(t, http.StatusForbidden, status, command)
		}
	}
	require.Equal(t, http.StatusForbidden, resolveBatchOnlyCommand("not_a_command"), "an unknown command fails closed")
}

func TestListenerPolicyRouteValidation(t *testing.T) {
	base := func() ListenerPolicy {
		return ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth}
	}
	for _, route := range []string{"", "full", "batch"} {
		policy := base()
		policy.Route = route
		require.NoError(t, policy.Validate(), "route %q", route)
	}
	policy := base()
	policy.Route = "bogus"
	require.ErrorContains(t, policy.Validate(), "unknown route")

	policy = base()
	policy.Route = "batch"
	policy.Pins = map[string]string{"abcdef12": "s1"}
	require.ErrorContains(t, policy.Validate(), "cannot be combined with session pins")
	policy = base()
	policy.Route = "batch"
	policy.PinsDeclared = true
	require.ErrorContains(t, policy.Validate(), "cannot be combined with session pins")
}
