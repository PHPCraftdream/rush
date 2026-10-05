package session

import (
	"context"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// noticeText is the exact frame shape agent.childQuestionNoticeText writes:
// the SUB-AGENT QUESTION frame inside a larger preamble.
func noticeText(childID, question string) string {
	return "Supervision summary.\n\nSUB-AGENT QUESTION (session " + childID + "): QUESTION: " + question +
		"\n\nThe sub-agent is paused on this question."
}

// TestParseChildQuestionNotice: the frame is found inside the preamble, the
// child id and question extracted, the question collapsed to one line.
func TestParseChildQuestionNotice(t *testing.T) {
	t.Parallel()

	q, ok := ParseChildQuestionNotice(noticeText("child-1", "ship\nor hold?"), "delegate-1")
	require.True(t, ok)
	require.Equal(t, "child-1", q.ChildSessionID)
	require.Equal(t, "delegate-1", q.DelegationToolCallID)
	require.Equal(t, "QUESTION: ship or hold?", q.Question)
	require.Equal(t, int64(0), q.NoticeID)

	_, ok = ParseChildQuestionNotice("no frame here", "delegate-1")
	require.False(t, ok)
	_, ok = ParseChildQuestionNotice("SUB-AGENT QUESTION (session ): empty", "j")
	require.False(t, ok)
}

// TestOneLineQuestionCapsByRunes: the cap counts runes, never bytes, and
// never splits a multi-byte character.
func TestOneLineQuestionCapsByRunes(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("ж", ChildQuestionDisplayMaxLen+10)
	got := OneLineQuestion(long)
	require.LessOrEqual(t, len([]rune(got)), ChildQuestionDisplayMaxLen+1)
	require.True(t, strings.HasSuffix(got, "…"))
}

// TestPendingChildQuestions: only the owner's pending child_question rows
// come back, parsed; other kinds and other owners stay out (#1158).
// Revert-check: dropping the kind filter or the owner predicate in
// PendingChildQuestions breaks the assertions.
func TestPendingChildQuestions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	store := NewAsyncJobStore(conn, dataDir, 1, "test")
	t.Cleanup(func() { _ = store.Close(ctx) })

	// session_notices.owner is an FK: real session rows first.
	svc := NewService(db.New(conn), conn)
	root1, err := svc.Create(ctx, "awaiting root")
	require.NoError(t, err)
	root2, err := svc.Create(ctx, "other root")
	require.NoError(t, err)

	// The void filter drops a notice whose bound job row is missing (the
	// pull path's rule), so the fixture claims its running delegation.
	_, err = store.Claim(ctx, ClaimParams{
		Owner: root1.ID, ToolCallID: "delegate-1", Kind: JobKindAgent,
		Input: "ask", ToolName: "agent", ChildSessionID: "child-1",
	})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, root1.ID, NoticeKindChildQuestion, noticeText("child-1", "go on?"), true, "delegate-1"))
	require.NoError(t, store.InsertSessionNotice(ctx, root1.ID, NoticeKindSupervision, "supervision text", true, ""))
	require.NoError(t, store.InsertSessionNotice(ctx, root2.ID, NoticeKindChildQuestion, noticeText("child-2", "foreign"), true, "delegate-2"))

	qs, err := store.PendingChildQuestions(ctx, root1.ID)
	require.NoError(t, err)
	require.Len(t, qs, 1)
	require.Equal(t, "child-1", qs[0].ChildSessionID)
	require.Equal(t, "delegate-1", qs[0].DelegationToolCallID)
	require.Equal(t, "QUESTION: go on?", qs[0].Question)
}

// TestClassifySessionActivity_ChildQuestionsAdditive: facts carrying
// child questions keep the Delegating kind but name the wait ("answer" in
// WaitingOn, the awaiting clause in the Description); without them the
// verdict is byte-for-byte the old one (#1158).
func TestClassifySessionActivity_ChildQuestionsAdditive(t *testing.T) {
	t.Parallel()

	base := ActivityFacts{LiveDelegations: 2}
	before := ClassifySessionActivity(base)

	withQ := base
	withQ.ChildQuestions = []ChildQuestion{{
		ChildSessionID: "child-9", DelegationToolCallID: "delegate-9", Question: "left or right?",
	}}
	after := ClassifySessionActivity(withQ)

	require.Equal(t, before.Kind, after.Kind, "display data must not change the Kind")
	require.Equal(t, before.PID, after.PID)
	require.Contains(t, after.WaitingOn, WaitAnswer)
	require.NotContains(t, before.WaitingOn, WaitAnswer)
	require.Contains(t, after.Description, "waiting for your answer: child child-9 asked: left or right?")
	require.NotContains(t, before.Description, "waiting for your answer")
}
