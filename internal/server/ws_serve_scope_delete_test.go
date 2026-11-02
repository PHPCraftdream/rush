package server

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func deletePayload(id string) json.RawMessage {
	raw, _ := json.Marshal(DeleteSessionPayload{SessionID: id})
	return raw
}

// TestServeWSDeleteSessionOwnership: a serve client may delete this
// workspace's own sessions, but a session bound to another checkout of a
// shared data directory is reported as not found and survives (design 17.5).
func TestServeWSDeleteSessionOwnership(t *testing.T) {
	f := newServeWSScopeFixture(t, false, map[string][]string{"run": {"run"}})
	ctx := t.Context()
	own, err := f.app.Sessions.Create(ctx, "own")
	require.NoError(t, err)
	foreignRoot := filepath.Join(t.TempDir(), "other-checkout")
	foreignSvc := session.NewServiceWithWorkspace(db.New(f.app.DB()), f.app.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignSvc.Create(ctx, "foreign")
	require.NoError(t, err)

	conn := f.dial(t, "run")

	reply, _ := exchange(t, conn, WSMessage{ID: "del-foreign", Type: CmdDeleteSession, Payload: deletePayload(foreign.ID)})
	require.Equal(t, EventError, reply.Type)
	require.Equal(t, "session not found", reply.Error)
	_, err = f.app.Sessions.Get(ctx, foreign.ID)
	require.NoError(t, err, "a foreign session must survive")

	reply, _ = exchange(t, conn, WSMessage{ID: "del-missing", Type: CmdDeleteSession, Payload: deletePayload("no-such-session")})
	require.Equal(t, EventError, reply.Type)
	require.Equal(t, "session not found", reply.Error)

	reply, _ = exchange(t, conn, WSMessage{ID: "del-own", Type: CmdDeleteSession, Payload: deletePayload(own.ID)})
	require.Equal(t, EventResponse, reply.Type, "own session deletion: %+v", reply)
	require.Empty(t, reply.Error)
	_, err = f.app.Sessions.Get(ctx, own.ID)
	require.Error(t, err, "the own session must be gone")
}

// TestUIDeleteSessionKeepsLegacyBehaviour documents that the loopback UI
// path (client without a serve identity) is deliberately unchanged by the
// serve-only ownership check: it still deletes whatever id it is given.
func TestUIDeleteSessionKeepsLegacyBehaviour(t *testing.T) {
	ctx := t.Context()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	foreignRoot := filepath.Join(t.TempDir(), "other-checkout")
	foreignSvc := session.NewServiceWithWorkspace(db.New(a.DB()), a.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignSvc.Create(ctx, "foreign")
	require.NoError(t, err)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 16)
	hub.register <- client

	handleDeleteSession(ctx, a, client, WSMessage{ID: "ui-del", Type: CmdDeleteSession, Payload: deletePayload(foreign.ID)})
	reply := replyByID(t, client, "ui-del")
	require.Equal(t, EventResponse, reply.Type)
	_, err = a.Sessions.Get(ctx, foreign.ID)
	require.Error(t, err, "legacy UI behaviour changed: the foreign session is still there")
}
