package server

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestHandleGetSessionLiveWork_AwaitingAnswerBadge: a pending child_question
// notice in session_notices must surface as the running delegation row's
// awaitingAnswer/awaitingQuestion wire fields (#1158); a delegation without
// such a notice stays clean. The notice is written through the same store
// the production path reads (db.New(a.DB()) session-notice inserts) in the
// legacy session-parentheses frame keyed by the held delegation's tool call
// id — batchPendingQuestions keeps that fallback keyed by JobToolCallID.
// Revert-check: removing the batchPendingQuestions read or the fill() match
// in buildSessionLiveWorkFromBatch leaves both fields zero, failing the
// first two assertions.
func TestHandleGetSessionLiveWork_AwaitingAnswerBadge(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	ctx := t.Context()

	root, err := a.Sessions.Create(ctx, "lw awaiting root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "lw-awaiting-child", root.ID, "worker")
	require.NoError(t, err)
	other, err := a.Sessions.CreateTaskSession(ctx, "lw-plain-child", root.ID, "worker")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: root.ID, toolCallID: "tc-await", taskKind: "agent", childSessionID: child.ID, state: "running"})
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: root.ID, toolCallID: "tc-plain", taskKind: "agent", childSessionID: other.ID, state: "running"})

	text := "Supervision summary.\n\nSUB-AGENT QUESTION (session " + child.ID + "): QUESTION: which adapter?" +
		"\n\nThe sub-agent is paused on this question."
	_, err = a.DB().ExecContext(ctx, `INSERT INTO session_notices
		(owner, kind, text, wake, delivery, reacted, job_tool_call_id, created_at, updated_at)
		VALUES (?, ?, ?, 1, 'pending', 0, 'tc-await', 1700000002, 1700000002)`,
		root.ID, session.NoticeKindChildQuestion, text)
	require.NoError(t, err)

	snap := requestSnapshot(t, a, client, root.ID)
	require.NotNil(t, snap)
	require.Len(t, snap.Agents, 2)
	byCall := map[string]struct {
		awaiting bool
		question string
	}{}
	for _, ag := range snap.Agents {
		byCall[ag.ToolCallID] = struct {
			awaiting bool
			question string
		}{ag.AwaitingAnswer, ag.AwaitingQuestion}
	}
	require.True(t, byCall["tc-await"].awaiting, "the delegation bound to the notice must carry the badge")
	require.Contains(t, byCall["tc-await"].question, "which adapter?")
	require.False(t, byCall["tc-plain"].awaiting, "a delegation without a pending question notice must stay clean")

	// Isolation: the notice is the OWNER's; the child's own snapshot does
	// not badge anything (the child owns no delegation rows).
	snap = requestSnapshot(t, a, client, child.ID)
	require.NotNil(t, snap)
	require.Empty(t, snap.Agents)
}
