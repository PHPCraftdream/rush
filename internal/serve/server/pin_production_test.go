package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pinFixture is the production serve listener (NewServeAPI + Configure +
// NewWithPolicy + a real token store + a real App) with a pinned token and an
// unpinned control token over two sessions of one workspace.
type pinFixture struct {
	base, endpoint string
	app            *appPkg.App
	server         *Server
	own, other     session.Session
	pinned         string
	control        string
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	isolateAPIGlobals(t)
	workingDir := t.TempDir()
	app := newAttachmentsTestApp(t, workingDir, t.TempDir())
	own, err := app.Sessions.Create(t.Context(), "pinned session")
	require.NoError(t, err)
	other, err := app.Sessions.Create(t.Context(), "other session")
	require.NoError(t, err)
	store := config.GlobalAPITokenStore()
	pinned, _, err := store.CreateAPITokenWithBinding("pinned", []string{"run"}, "ask", nil, config.APITokenBinding{
		Kind: "session", Pin: &config.APITokenPinBinding{Session: own.ID, Workspace: workingDir},
	})
	require.NoError(t, err)
	control, _, err := store.CreateAPIToken("control", []string{"run"}, "ask", nil)
	require.NoError(t, err)
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Workspace: workingDir,
		Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		Limits:   ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000},
	}
	api, err := NewServeAPI(policy, app, store, nil)
	require.NoError(t, err)
	api.Configure(&policy)
	srv := NewWithPolicy(app, policy, nil)
	base := startPolicyServer(t, srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, api.Stop(ctx))
	})
	return &pinFixture{base: base, endpoint: "ws" + strings.TrimPrefix(base, "http"), app: app, server: srv, own: own, other: other, pinned: pinned, control: control}
}

func (f *pinFixture) rest(t *testing.T, token, method, path string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, f.base+"/api/v1"+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// problemShape reduces a problem+json body to the fields that must be
// identical for a foreign and a nonexistent resource (no request id, no path).
func problemShape(t *testing.T, body string) string {
	t.Helper()
	var problem map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &problem), body)
	shape, err := json.Marshal(map[string]any{"type": problem["type"], "title": problem["title"], "status": problem["status"], "detail": problem["detail"]})
	require.NoError(t, err)
	return string(shape)
}

func pageSessionIDs(t *testing.T, body string) []string {
	t.Helper()
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page), body)
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestPinnedRESTConfinementThroughProductionListener(t *testing.T) {
	f := newPinFixture(t)

	// Visibility: the pinned token lists, reads and streams only its session.
	status, body := f.rest(t, f.pinned, http.MethodGet, "/sessions")
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, []string{f.own.ID}, pageSessionIDs(t, body))
	_, body = f.rest(t, f.control, http.MethodGet, "/sessions")
	require.ElementsMatch(t, []string{f.own.ID, f.other.ID}, pageSessionIDs(t, body), "the unpinned control token must see both")

	status, _ = f.rest(t, f.pinned, http.MethodGet, "/sessions/"+f.own.ID)
	require.Equal(t, http.StatusOK, status)
	status, _ = f.rest(t, f.pinned, http.MethodGet, "/sessions/"+f.own.ID+"/messages")
	require.Equal(t, http.StatusOK, status)

	foreignStatus, foreignBody := f.rest(t, f.pinned, http.MethodGet, "/sessions/"+f.other.ID)
	missingStatus, missingBody := f.rest(t, f.pinned, http.MethodGet, "/sessions/no-such-session")
	require.Equal(t, http.StatusNotFound, foreignStatus)
	require.Equal(t, http.StatusNotFound, missingStatus)
	require.Equal(t, problemShape(t, missingBody), problemShape(t, foreignBody), "a foreign session must be indistinguishable from a missing one")
	for _, path := range []string{"/messages", "/events"} {
		status, _ = f.rest(t, f.pinned, http.MethodGet, "/sessions/"+f.other.ID+path)
		require.Equal(t, http.StatusNotFound, status, path)
	}

	// Operation class: forbidden operations are 403 on the pinned session, 404 on a foreign one.
	status, body = f.rest(t, f.pinned, http.MethodPost, "/sessions")
	require.Equal(t, http.StatusForbidden, status, body)
	status, _ = f.rest(t, f.pinned, http.MethodPost, "/sessions/"+f.own.ID+"/fork")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = f.rest(t, f.pinned, http.MethodDelete, "/sessions/"+f.own.ID)
	require.Equal(t, http.StatusForbidden, status)
	status, _ = f.rest(t, f.pinned, http.MethodDelete, "/sessions/"+f.other.ID)
	require.Equal(t, http.StatusNotFound, status, "visibility is decided before the operation class")
	for _, id := range []string{f.own.ID, f.other.ID} {
		_, err := f.app.Sessions.Get(t.Context(), id)
		require.NoError(t, err, "a refused delete removed a session")
	}

	// Owned routes reach their handlers (and find nothing that is not the token's own).
	status, _ = f.rest(t, f.pinned, http.MethodGet, "/runs/no-such-run")
	require.Equal(t, http.StatusNotFound, status)
	status, _ = f.rest(t, f.pinned, http.MethodGet, "/permissions")
	require.NotEqual(t, http.StatusForbidden, status, "the permission list is the token's own and must stay reachable")

	// Positive control: the same delete succeeds for the unpinned token.
	status, body = f.rest(t, f.control, http.MethodDelete, "/sessions/"+f.other.ID)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}, status, body)
	_, err := f.app.Sessions.Get(t.Context(), f.other.ID)
	require.Error(t, err)
}

func TestPinnedWSCommandsConfinementThroughProductionListener(t *testing.T) {
	f := newPinFixture(t)
	fixture := &scopeFixture{endpoint: f.endpoint, app: f.app, server: f.server, tokens: map[string]string{"pinned": f.pinned}}
	conn := fixture.dial(t, "pinned")

	reply, _ := exchange(t, conn, WSMessage{ID: "list", Type: CmdListSessions})
	require.Equal(t, EventSessionsList, reply.Type)
	var listed []session.Session
	require.NoError(t, json.Unmarshal(reply.Payload, &listed))
	require.Len(t, listed, 1)
	require.Equal(t, f.own.ID, listed[0].ID)

	payload := func(id string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"sessionID": id})
		return raw
	}
	cases := []struct {
		name, command, session string
		wantType, wantError    string
	}{
		{"own read", CmdLoadMessages, f.own.ID, EventMessagesList, ""},
		{"foreign read is hidden", CmdLoadMessages, f.other.ID, EventError, "not found"},
		{"nonexistent read is hidden the same way", CmdLoadMessages, "no-such-session", EventError, "not found"},
		{"foreign work is hidden", CmdSendMessage, f.other.ID, EventError, "not found"},
		{"foreign forbidden operation is hidden before it is forbidden", CmdDeleteSession, f.other.ID, EventError, "not found"},
		{"own forbidden operation", CmdDeleteSession, f.own.ID, EventError, "forbidden: pinned-token"},
		{"fork is forbidden", CmdForkSession, f.own.ID, EventError, "forbidden: pinned-token"},
	}
	for _, tc := range cases {
		reply, _ := exchange(t, conn, WSMessage{ID: tc.name, Type: tc.command, Payload: payload(tc.session)})
		require.Equal(t, tc.wantType, reply.Type, tc.name)
		require.Equal(t, tc.wantError, reply.Error, tc.name)
	}
	for _, command := range []string{CmdCreateSession, CmdGetConfig, CmdGetLogs, CmdShutdownServer, "definitely_not_a_command"} {
		reply, _ := exchange(t, conn, WSMessage{ID: command, Type: command, Payload: json.RawMessage(`{}`)})
		require.Equal(t, EventError, reply.Type, command)
		require.Equal(t, "forbidden: pinned-token", reply.Error, command)
	}
	for _, id := range []string{f.own.ID, f.other.ID} {
		_, err := f.app.Sessions.Get(t.Context(), id)
		require.NoError(t, err, "a refused command changed a session")
	}
}
