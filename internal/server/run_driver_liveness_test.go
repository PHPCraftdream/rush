package server

// R5C-5, web half: a session a live `rush run` loop drives between turns (a
// paced Drain retry after a provider 5xx, debt pending) has no lock and no
// running row; the durable driver marker is the only cross-process fact that
// its scope is open. The sessions_list reply must not show it as idle.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: dropping the driver FACT (Facts.Driver) from
// annotateSessionActivity's HasLiveOwnWork mapping fails the "driven"
// assertion (HasLiveOwnWork stays false).
func TestHandleListSessions_LiveRunDriverAnnotation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	driven, err := a.Sessions.Create(ctx, "driven by a rush run loop")
	require.NoError(t, err)
	idle, err := a.Sessions.Create(ctx, "idle")
	require.NoError(t, err)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	list := func() []session.Session {
		handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
		replies := drainSessionsListReplies(t, client)
		require.NotEmpty(t, replies, "handleListSessions must reply with a sessions_list event")
		return replies[len(replies)-1]
	}

	rows := list()
	require.False(t, findSessionRow(t, rows, driven.ID).HasLiveOwnWork, "no marker: idle")

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))

	rows = list()
	require.True(t, findSessionRow(t, rows, driven.ID).HasLiveOwnWork,
		"a live rush run driver between turns keeps the session from reading as idle")
	require.False(t, findSessionRow(t, rows, idle.ID).HasLiveOwnWork, "another session is unaffected")

	require.NoError(t, store.ReleaseSessionDriver(ctx, driven.ID))
	require.False(t, findSessionRow(t, list(), driven.ID).HasLiveOwnWork, "the loop exited: idle again")
}
