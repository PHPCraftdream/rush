package server

// WS-1 ownership on the web's DESTRUCTIVE actions (#1142 step C, §1 of
// docs/plans/2026-10-01-shared-data-dir.md): "web: rerun" refuses a foreign
// session before TruncateForRerun, and "web: delete other sessions" deletes
// only rows this workspace owns. Both decisions read the process's workspace
// through appOwnsSession (session.Owns) — the same predicate the run path
// applies — so in a shared data directory one checkout can never drive or wipe
// another's sessions.
//
// send / interrupt / inject are intentionally NOT touched here: WS-1 guards
// those in the agent layer (runOwned / refuseForeignWorkspace), and an inject
// legitimately writes a message the OWNING process goes on to execute.

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestHandleRerunMessage_OwnSessionStillReruns: the ownership gate must not
// block a session this process owns. The rerun-handler fixture creates its
// session through the app's own (home) service, so it is a legacy ” row a home
// process owns — the rerun must still reach its truncation.
//
// Revert-check: making the guard refuse unconditionally (or bailing out for
// every session) turns this EventResponse into an EventError reply and the
// truncateCalls == 1 assertion red.
func TestHandleRerunMessage_OwnSessionStillReruns(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-own-ws1")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord

	var truncateCalls int
	prev := rerunTruncate
	rerunTruncate = func(context.Context, *sql.DB, message.Service, session.RerunTruncateParams) (session.RerunTruncation, error) {
		truncateCalls++
		return session.RerunTruncation{}, nil
	}
	t.Cleanup(func() { rerunTruncate = prev })

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type, "an owned session must still rerun")
	require.Equal(t, 1, truncateCalls, "the ownership gate must not stop an owned session's truncation")
}

// TestHandleRerunMessage_ForeignWorkspaceRefusedHistoryIntact: rerunning a
// session bound to ANOTHER workspace (visible only because the data directory
// is shared) is refused, and the truncation transaction is NEVER reached — the
// target, the tail, the job's announce message and the job row all survive
// byte-for-byte, and not even the live turn is cancelled.
//
// Revert-check: removing the `if ... !appOwnsSession(a, sess)` block in
// handleRerunMessage (internal/server/handlers_agent_rerun.go) lets the rerun
// proceed: truncateCalls becomes 1 and cancelTurn becomes >=1 — the foreign
// session's history is destroyed. require.Zero(truncateCalls) (this file) goes
// red first.
func TestHandleRerunMessage_ForeignWorkspaceRefusedHistoryIntact(t *testing.T) {
	f := newRerunHandlerFx(t, "rerun-foreign-ws1")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord

	// Re-bind the fixture's session to a checkout this process is not — exactly
	// what a row created by another worktree's process looks like here. (The
	// app's own working dir is a non-git temp dir, i.e. a home process, so the
	// non-empty foreign root fails appOwnsSession.)
	foreignRoot := filepath.Join(t.TempDir(), "other-checkout")
	_, err := f.a.DB().ExecContext(t.Context(),
		`UPDATE sessions SET workspace_root = ?, git_branch = 'feature-x' WHERE id = ?`,
		foreignRoot, f.sessionID)
	require.NoError(t, err)

	truncateCalls := 0
	prev := rerunTruncate
	rerunTruncate = func(context.Context, *sql.DB, message.Service, session.RerunTruncateParams) (session.RerunTruncation, error) {
		truncateCalls++
		return session.RerunTruncation{}, nil
	}
	t.Cleanup(func() { rerunTruncate = prev })

	env := f.run(t)

	require.Equal(t, EventError, env.Type, "rerunning another workspace's session must be refused")
	require.Contains(t, env.Error, "belongs to", "the refusal states the ownership condition")
	require.Contains(t, env.Error, foreignRoot, "the refusal names the owning workspace")
	require.Contains(t, env.Error, "rush sessions fork", "the refusal offers the sanctioned way forward")
	require.Zero(t, truncateCalls, "TruncateForRerun must NOT run for a foreign session — history stays intact")
	cancelTurn, _, _, _, _ := mockCoord.counters()
	require.Zero(t, cancelTurn, "not even the live turn is cancelled for a foreign session")

	require.True(t, f.exists(t, f.target.ID), "the target message must survive a refused rerun")
	require.True(t, f.exists(t, f.tail.ID), "the tail must survive a refused rerun")
	require.True(t, f.exists(t, f.started.ID), "the job's announce message must survive")
	require.Equal(t, "none", f.jobDelivery(t), "the job must not be voided by a refused rerun")
}

// recordingDeleteSessionService wraps a real session.Service and records every
// Delete ID, delegating everything else, so a test can prove which sessions
// handleDeleteOtherSessions actually tried to remove.
type recordingDeleteSessionService struct {
	session.Service
	mu      sync.Mutex
	deleted []string
}

func (r *recordingDeleteSessionService) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	r.deleted = append(r.deleted, id)
	r.mu.Unlock()
	return r.Service.Delete(ctx, id)
}

func (r *recordingDeleteSessionService) deletedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

// replyByID drains the client's send channel until it sees the reply for the
// given request ID (handleDeleteOtherSessions also emits unrelated broadcasts).
func replyByID(t *testing.T, c *Client, id string) WSMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case raw := <-c.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.ID == id {
				return env
			}
		case <-time.After(50 * time.Millisecond):
			require.True(t, time.Now().Before(deadline), "no reply with id %q arrived", id)
		}
	}
}

// TestHandleDeleteOtherSessions_SkipsForeignWorkspace: in a shared data
// directory the client's "delete all others" must remove only THIS workspace's
// own rows — its own sessions, plus legacy ” rows when this is a home process.
// A session bound to another checkout must be skipped (Delete is never even
// called for it) and must survive. The kept session is never touched either.
//
// Revert-check: removing the `if !appOwnsSession(a, s)` guard in
// handleDeleteOtherSessions (internal/server/handlers_sessions.go) makes Delete
// be called for the foreign session: require.NotContains(deleted, foreign.ID)
// and the "foreign must survive" Get both fail.
func TestHandleDeleteOtherSessions_SkipsForeignWorkspace(t *testing.T) {
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	keep, err := a.Sessions.Create(ctx, "keep-me")
	require.NoError(t, err)
	mine, err := a.Sessions.Create(ctx, "my-own-session") // legacy '' row, home-owned
	require.NoError(t, err)

	// A session bound to another checkout, written straight into the shared DB
	// the way that checkout's own process would have. A home process refuses it.
	foreignRoot := filepath.Join(t.TempDir(), "other-checkout")
	foreignSvc := session.NewServiceWithWorkspace(db.New(a.DB()), a.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignSvc.Create(ctx, "another checkout's session")
	require.NoError(t, err)
	require.Equal(t, foreignRoot, foreign.WorkspaceRoot)

	rec := &recordingDeleteSessionService{Service: a.Sessions}
	a.Sessions = rec

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	payload, err := json.Marshal(DeleteOtherSessionsPayload{KeepID: keep.ID})
	require.NoError(t, err)
	handleDeleteOtherSessions(ctx, a, client, WSMessage{ID: "req-ws1-del", Type: CmdDeleteOtherSessions, Payload: payload})

	reply := replyByID(t, client, "req-ws1-del")
	require.Equal(t, EventResponse, reply.Type)

	var result DeleteOtherSessionsResult
	require.NoError(t, json.Unmarshal(reply.Payload, &result))

	require.ElementsMatch(t, []string{mine.ID}, result.DeletedIDs, "only this workspace's own row is deleted")
	require.Empty(t, result.FailedIDs)
	require.NotContains(t, result.DeletedIDs, foreign.ID)
	require.NotContains(t, rec.deletedIDs(), foreign.ID,
		"Delete must never be called for another workspace's session")
	require.NotContains(t, rec.deletedIDs(), keep.ID, "the kept session is never deleted")

	// The foreign session survives in the shared DB; this workspace's own is gone.
	_, err = rec.Get(ctx, foreign.ID)
	require.NoError(t, err, "a foreign-workspace session must survive another workspace's delete-other")
	_, err = rec.Get(ctx, mine.ID)
	require.Error(t, err, "this workspace's own session is deleted")
	keptStill, err := rec.Get(ctx, keep.ID)
	require.NoError(t, err)
	require.Equal(t, keep.ID, keptStill.ID)
}

// wsGitRepo is a real git repository, so the fixture app's workspace root is
// non-empty (mirrors the app/agent tests' git-init pattern): the stand-in for
// "a process running inside a checkout".
func wsGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	cmd := platform.Command(t.Context(), "git", "init", "-q")
	cmd.Dir = repo
	require.NoError(t, cmd.Run())
	return repo
}

// TestWS1_LegacyRowsAreOwnedByGitCheckoutServer is the server half of the
// SD-C home-vs-root regression (#1142 step C): a process running inside a
// git checkout has a NON-EMPTY workspace root and is still a HOME process
// today (config.WorkspaceHome() == true until SD-D #1143), so its legacy ”
// rows -- the shape every pre-WS-1 row has -- must stay drivable and
// deletable by BOTH destructive web actions. With home derived from the root
// (the first draft's `ws == ""`) rerun refused them and delete-other skipped
// them: the operator's own history became un-rerunnable and
// "delete others" silently stopped working inside any checkout.
//
// Revert-check (mutant C): in appOwnsSession
// (internal/server/handlers_sessions.go) restore the root-derived home --
// `session.Owns(sess.WorkspaceRoot, ws, ws == "")` -- both sub-tests fail:
// rerun replies EventError instead of EventResponse (truncateCalls stays 0,
// the require.Equal(t, 1, ...) below), and delete-other's DeletedIDs comes
// back empty (require.Len fails).
func TestWS1_LegacyRowsAreOwnedByGitCheckoutServer(t *testing.T) {
	t.Run("rerun of a legacy row still reruns", func(t *testing.T) {
		// The fixture app runs inside a real checkout: non-empty workspace
		// root, home flag unchanged (the app's own -- home, until SD-D).
		f := newRerunHandlerFxIn(t, wsGitRepo(t), "rerun-legacy-git-checkout")
		f.a.AgentCoordinator = &cancellableHoldCoordinator{}

		// The fixture creates the session bound to THIS checkout's root (its
		// own service); re-bind it to the pre-WS-1 shape -- workspace_root
		// '' -- which is what the rerun target must be for this regression.
		_, err := f.a.DB().ExecContext(t.Context(),
			`UPDATE sessions SET workspace_root = '' WHERE id = ?`, f.sessionID)
		require.NoError(t, err)
		row, err := f.a.Sessions.Get(t.Context(), f.sessionID)
		require.NoError(t, err)
		require.Empty(t, row.WorkspaceRoot, "fixture: the rerun target is a legacy row")

		var truncateCalls int
		prev := rerunTruncate
		rerunTruncate = func(context.Context, *sql.DB, message.Service, session.RerunTruncateParams) (session.RerunTruncation, error) {
			truncateCalls++
			return session.RerunTruncation{}, nil
		}
		t.Cleanup(func() { rerunTruncate = prev })

		env := f.run(t)
		require.Equal(t, EventResponse, env.Type,
			"a legacy row is owned by a git-checkout home process: rerun must proceed")
		require.Equal(t, 1, truncateCalls)
	})

	t.Run("delete other still deletes legacy rows", func(t *testing.T) {
		a := newAttachmentsTestApp(t, wsGitRepo(t), t.TempDir())
		ctx := t.Context()

		keep, err := a.Sessions.Create(ctx, "keep-me")
		require.NoError(t, err)
		// A legacy row: written by a home service over the same shared DB —
		// the pre-WS-1 shape this app owned before WS-1 existed.
		legacySvc := session.NewService(db.New(a.DB()), a.DB())
		legacy, err := legacySvc.Create(ctx, "legacy row of this checkout")
		require.NoError(t, err)
		require.Empty(t, legacy.WorkspaceRoot, "fixture: created by a home service, pre-WS-1 shape")

		hub := newHub()
		go hub.Run(ctx)
		client := newClient(hub, nil)
		client.send = make(chan []byte, 100)
		hub.register <- client

		payload, err := json.Marshal(DeleteOtherSessionsPayload{KeepID: keep.ID})
		require.NoError(t, err)
		handleDeleteOtherSessions(ctx, a, client, WSMessage{ID: "req-ws1-legacy-del", Type: CmdDeleteOtherSessions, Payload: payload})

		reply := replyByID(t, client, "req-ws1-legacy-del")
		require.Equal(t, EventResponse, reply.Type)
		var result DeleteOtherSessionsResult
		require.NoError(t, json.Unmarshal(reply.Payload, &result))
		require.Contains(t, result.DeletedIDs, legacy.ID,
			"a legacy row is owned by a git-checkout home process: delete other must delete it")
		_, err = a.Sessions.Get(ctx, legacy.ID)
		require.Error(t, err, "the legacy row must be gone")
	})
}
