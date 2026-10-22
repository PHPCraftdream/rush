package server

// Wiring test for the cross-process descendant-work signal on the web
// session list: a top-level session whose own lock is gone while a
// sub-agent session below it still has a LIVE delegation (a live batch
// root naming the child session) is NOT finished, and the sessions_list
// reply must say so.
//
// This is the web half of the same bug `rush sessions list` / `sessions
// why` regressions cover: the coordinator's in-process parked-delegation
// registry is invisible to a process that does not own the delegation, so
// the durable state (a live batch delegation root with its child_session_id
// link) is the only signal that crosses the process boundary. The
// derivation itself is the batch live-walk (BatchLiveForSessions) — the
// same helper markDelegatingLiveDescendants and explainSessionStatus read
// through — so this test only has to prove the wiring feeds the row.
//
// The delegation root is reserved through the production
// session.BatchStore.Reserve API (see seedLiveWorkRow in
// livework_seed_test.go): a real sqlite-backed tree of batch_nodes, read
// exactly the way a row owned by another process would be read.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// drainSessionsListReplies collects every sessions_list reply currently
// buffered on the client's send channel. handleListSessions may also emit
// agent_busy / summarize_queued corrections when a coordinator is
// configured, so the replies are filtered by type.
func drainSessionsListReplies(t *testing.T, client *Client) [][]session.Session {
	t.Helper()
	var out [][]session.Session
	for {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			if env.Type != EventSessionsList {
				continue
			}
			var sessions []session.Session
			require.NoError(t, json.Unmarshal(env.Payload, &sessions))
			out = append(out, sessions)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

// findSessionRow returns the row for sessionID from a sessions_list reply.
func findSessionRow(t *testing.T, sessions []session.Session, sessionID string) session.Session {
	t.Helper()
	for _, s := range sessions {
		if s.ID == sessionID {
			return s
		}
	}
	t.Fatalf("session %s missing from the sessions_list reply", sessionID)
	return session.Session{}
}

func TestHandleListSessions_LiveDescendantWorkAnnotation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	parent, err := a.Sessions.Create(ctx, "parent waiting on sub-agent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "ws-desc-child", parent.ID, "implementation sub-agent")
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

	// Negative control: the child session row exists but no live delegation
	// root names it, so the parent is genuinely idle.
	rows := list()
	parentRow := findSessionRow(t, rows, parent.ID)
	require.False(t, parentRow.HasLiveDescendantWork,
		"a child row with no live delegation must not mark the parent as waiting on work")
	require.Empty(t, parentRow.LiveDescendantIDs)

	// A REAL running delegation (owner=parent, child_session_id=child) as a
	// live batch root: the parent must now be annotated with live descendant
	// work, naming the child.
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: parent.ID, toolCallID: "delegate-1", taskKind: "agent", childSessionID: child.ID, state: "running"})

	rows = list()
	parentRow = findSessionRow(t, rows, parent.ID)
	require.True(t, parentRow.HasLiveDescendantWork,
		"a live delegation must reach the parent row through the sessions_list reply — the web half of the done-while-delegating bug")
	require.Equal(t, []string{child.ID}, parentRow.LiveDescendantIDs,
		"the annotation must name the descendant session the UI is waiting on")
	require.False(t, parentRow.OwnedExternal,
		"the parent itself holds no lock: descendant work is not external ownership of the parent")

	// The child is not itself listed — the list is top-level sessions only.
	for _, s := range rows {
		require.NotEqual(t, child.ID, s.ID, "sub-agent sessions are not top-level rows")
	}

	// The delegation root reaches a terminal state: the parent returns to
	// idle.
	projectLiveWorkRoot(t, a, parent.ID, "delegate-1", "completed")

	rows = list()
	parentRow = findSessionRow(t, rows, parent.ID)
	require.False(t, parentRow.HasLiveDescendantWork,
		"once the delegation root is terminal the parent must read as idle again")
	require.Empty(t, parentRow.LiveDescendantIDs)
}

// TestHandleListSessions_LiveOwnWorkAnnotation: a top-level session whose
// scope is open only because of its OWN running plain job (no delegation,
// no lock between turns) must not read as idle in the sessions_list reply --
// the web half of the `sessions list`/`sessions why` "running, not done"
// promotion. HasLiveDescendantWork stays false: nothing below it is working.
//
// Revert-check performed: dropped the HasLiveOwnWork assignment from
// handleListSessions -- the HasLiveOwnWork assertion FAILED.
func TestHandleListSessions_LiveOwnWorkAnnotation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx := t.Context()

	root, err := a.Sessions.Create(ctx, "root waiting on its own job")
	require.NoError(t, err)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client

	list := func() session.Session {
		handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
		replies := drainSessionsListReplies(t, client)
		require.NotEmpty(t, replies, "handleListSessions must reply with a sessions_list event")
		return findSessionRow(t, replies[len(replies)-1], root.ID)
	}

	require.False(t, list().HasLiveOwnWork, "no job: idle")

	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: root.ID, toolCallID: "bash-1", taskKind: "bash", state: "running"})

	row := list()
	require.True(t, row.HasLiveOwnWork, "a running own plain job must reach the row: the session is not idle")
	require.False(t, row.HasLiveDescendantWork, "no delegation, so nothing below the root is working")
	require.Empty(t, row.LiveDescendantIDs)

	projectLiveWorkRoot(t, a, root.ID, "bash-1", "completed")
	require.False(t, list().HasLiveOwnWork, "a terminal job no longer keeps the session working")
}
