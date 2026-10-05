package server

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestHandleGetSessionLiveWork_AwaitingAnswerBadge: a pending child_question
// notice bound to a running delegation row must surface as the row's
// awaitingAnswer/awaitingQuestion wire fields (#1158); a delegation without
// such a notice stays clean. Revert-check: removing the PendingChildQuestions
// read or the fill() match in buildSessionLiveWork leaves both fields zero,
// failing the first two assertions.
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
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: root.ID, ToolCallID: "tc-await", Kind: session.JobKindAgent, Input: "investigate", ToolName: "agent", ChildSessionID: child.ID})
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: root.ID, ToolCallID: "tc-plain", Kind: session.JobKindAgent, Input: "also work", ToolName: "agent", ChildSessionID: other.ID})
	require.NoError(t, err)

	text := "Supervision summary.\n\nSUB-AGENT QUESTION (session " + child.ID + "): QUESTION: which adapter?" +
		"\n\nThe sub-agent is paused on this question."
	require.NoError(t, store.InsertSessionNotice(ctx, root.ID, session.NoticeKindChildQuestion, text, true, "tc-await"))

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
