package server

// Wiring test for the cross-process descendant-work signal on the web
// session list: a top-level session whose own lock is gone while a
// sub-agent session below it still holds a LIVE lock is NOT finished, and
// the sessions_list reply must say so.
//
// This is the web half of the same bug `rush sessions list` / `sessions
// why` regressions cover: the coordinator's in-process parked-delegation
// registry is invisible to a process that does not own the delegation, so
// the durable state (child rows via parent_session_id + the children's own
// locks) is the only signal that crosses the process boundary. The
// derivation itself is session.LiveDescendants — the same helper
// markDelegatingLiveDescendants and explainSessionStatus use — so this test
// only has to prove the wiring feeds the row.
//
// The child's lock is a REAL exclusive lock acquired in-process via
// session.TryAcquireSessionLock (released through defer), not a forged
// file: the annotation path reads a genuine held lock exactly the way it
// would read one held by another process.

import (
	"encoding/json"
	"os"
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

	// Negative control: the child session row exists but holds no lock, so
	// the parent is genuinely idle.
	rows := list()
	parentRow := findSessionRow(t, rows, parent.ID)
	require.False(t, parentRow.HasLiveDescendantWork,
		"a child row with no live lock must not mark the parent as waiting on work")
	require.Empty(t, parentRow.LiveDescendantIDs)

	// The child takes a REAL held lock: the parent must now be annotated
	// with live descendant work, naming the child.
	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)

	rows = list()
	parentRow = findSessionRow(t, rows, parent.ID)
	require.True(t, parentRow.HasLiveDescendantWork,
		"a live sub-agent lock must reach the parent row through the sessions_list reply — the web half of the done-while-delegating bug")
	require.Equal(t, []string{child.ID}, parentRow.LiveDescendantIDs,
		"the annotation must name the descendant session the UI is waiting on")
	require.False(t, parentRow.OwnedExternal,
		"the parent itself holds no lock: descendant work is not external ownership of the parent")

	// The child is not itself listed — the list is top-level sessions only.
	for _, s := range rows {
		require.NotEqual(t, child.ID, s.ID, "sub-agent sessions are not top-level rows")
	}

	// Release the child's lock and age it out: the parent returns to idle.
	require.NoError(t, childLock.Release())
	releasedAgo := time.Now().Add(-(session.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(session.SessionLockPath(dataDir, child.ID), releasedAgo, releasedAgo))

	rows = list()
	parentRow = findSessionRow(t, rows, parent.ID)
	require.False(t, parentRow.HasLiveDescendantWork,
		"once the descendant lock is gone the parent must read as idle again")
	require.Empty(t, parentRow.LiveDescendantIDs)
}
