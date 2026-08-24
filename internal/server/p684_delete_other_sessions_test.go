package server

// Task #684 (F-1, P3, docs/reviews/2026-08-24-twenty-seventh-review-beed48a4.md):
// handleDeleteOtherSessions used to swallow a per-session delete failure
// with only a slog.Warn and still reply an unqualified
// EventResponse{"status":"ok"} — the client had no way to learn which
// session (if any) survived, so it removed every non-kept row from the
// sidebar unconditionally on that lossy "ok" (the client-side half of this
// fix, in web/src/components/Sidebar.tsx, now only drops rows whose ID is
// echoed back in deletedIDs).
//
// This test forces one of two non-kept sessions to fail its Delete call
// deterministically (no timing/race dependency) via a thin wrapper around
// the real session.Service that fails Delete for one chosen ID and
// otherwise delegates unchanged, and asserts the reply payload correctly
// separates deletedIDs from failedIDs.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// failOnDeleteSessionService wraps a real session.Service and fails Delete
// for exactly one session ID, delegating every other call (including every
// other ID's Delete) to the embedded real service unchanged.
type failOnDeleteSessionService struct {
	session.Service
	failID string
}

func (f *failOnDeleteSessionService) Delete(ctx context.Context, id string) error {
	if id == f.failID {
		return errors.New("simulated: database is locked")
	}
	return f.Service.Delete(ctx, id)
}

func TestHandleDeleteOtherSessions_PartialFailureReportsBothLists(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	keep, err := a.Sessions.Create(ctx, "keep-me")
	require.NoError(t, err)
	ok, err := a.Sessions.Create(ctx, "deletes-ok")
	require.NoError(t, err)
	fails, err := a.Sessions.Create(ctx, "fails-server-side")
	require.NoError(t, err)

	a.Sessions = &failOnDeleteSessionService{Service: a.Sessions, failID: fails.ID}

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	payload, err := json.Marshal(DeleteOtherSessionsPayload{KeepID: keep.ID})
	require.NoError(t, err)
	handleDeleteOtherSessions(ctx, a, client, WSMessage{ID: "req-684", Type: CmdDeleteOtherSessions, Payload: payload})

	var reply *WSMessage
	deadline := time.Now().Add(2 * time.Second)
	for reply == nil && time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID != "req-684" {
				continue
			}
			reply = &env
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.NotNil(t, reply, "handler must reply to the request")
	require.Equal(t, EventResponse, reply.Type, "a partial failure must still be an EventResponse, not EventError, so the client can inspect which sessions survived")

	var result DeleteOtherSessionsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))

	require.ElementsMatch(t, []string{ok.ID}, result.DeletedIDs,
		"only the session whose Delete genuinely succeeded must be reported as deleted")
	require.ElementsMatch(t, []string{fails.ID}, result.FailedIDs,
		"the session whose Delete failed server-side must be reported as failed, not silently folded into an unqualified ok")

	// Verify against real DB state: "ok" is actually gone, "fails" is not.
	_, err = a.Sessions.Get(ctx, ok.ID)
	require.Error(t, err, "the successfully deleted session must actually be gone from the DB")
	still, err := a.Sessions.Get(ctx, fails.ID)
	require.NoError(t, err, "the session whose delete failed must still exist server-side")
	require.Equal(t, fails.ID, still.ID)

	// The kept session was never touched.
	keptStill, err := a.Sessions.Get(ctx, keep.ID)
	require.NoError(t, err)
	require.Equal(t, keep.ID, keptStill.ID)
}

func TestHandleDeleteOtherSessions_FullSuccessReportsAllDeletedNoneFailed(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	keep, err := a.Sessions.Create(ctx, "keep-me")
	require.NoError(t, err)
	other1, err := a.Sessions.Create(ctx, "other-1")
	require.NoError(t, err)
	other2, err := a.Sessions.Create(ctx, "other-2")
	require.NoError(t, err)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	payload, err := json.Marshal(DeleteOtherSessionsPayload{KeepID: keep.ID})
	require.NoError(t, err)
	handleDeleteOtherSessions(ctx, a, client, WSMessage{ID: "req-684-ok", Type: CmdDeleteOtherSessions, Payload: payload})

	var reply *WSMessage
	deadline := time.Now().Add(2 * time.Second)
	for reply == nil && time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID != "req-684-ok" {
				continue
			}
			reply = &env
		case <-time.After(50 * time.Millisecond):
		}
	}
	require.NotNil(t, reply)
	require.Equal(t, EventResponse, reply.Type)

	var result DeleteOtherSessionsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))
	require.ElementsMatch(t, []string{other1.ID, other2.ID}, result.DeletedIDs)
	require.Empty(t, result.FailedIDs, "control: nothing failed, FailedIDs must be empty")

	_, err = a.Sessions.Get(ctx, other1.ID)
	require.Error(t, err)
	_, err = a.Sessions.Get(ctx, other2.ID)
	require.Error(t, err)
}
