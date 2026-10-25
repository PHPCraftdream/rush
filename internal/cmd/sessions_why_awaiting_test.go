package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// whyAwaitingTestEnv builds the hand-backed App explainWhy drives, plus the
// SessionNoticeStore the held-question reader goes through.
func whyAwaitingTestEnv(t *testing.T) (*app.App, *session.SessionNoticeStore, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetDataDirForTest(dataDir)
	return a, session.NewSessionNoticeStore(conn), dataDir
}

// insertChildQuestionNotice writes one child_question notice in the exact
// shape agent.childQuestionNoticeText produces (frame inside a preamble),
// bound to the delegation's tool call id.
func insertChildQuestionNotice(t *testing.T, store *session.SessionNoticeStore, owner, childID, jobID, question string) {
	t.Helper()
	text := "Supervision summary.\n\nSUB-AGENT QUESTION (session " + childID + "): QUESTION: " + question +
		"\n\nThe sub-agent is paused on this question.\nAnswer: call `agent` with resume_session_id=\"" + childID + "\"."
	require.NoError(t, store.InsertSessionNotice(context.Background(), owner, session.NoticeKindChildQuestion, text, true, jobID))
}

// TestExplainSessionStatus_AwaitingAnswerNamesQuestion: a held batch
// delegation with a bound child_question notice must surface the awaiting-
// answer section naming the child that asked and its question (parsed from
// the bound notice's canonical frame), the in-process answer path
// (inject_agent + child_session_id), the delegation's tool call under
// "Batch roots:", and the batch live verdict (never at rest).
//
// Revert-check: dropping the bound-notice check in reduceBatchSessionFacts,
// the frame parse, or the renderBatchQuestionNote lines fails the first
// assertions; reporting the session at rest fails the last.
// It covers the BOUND (legacy-shaped) variant: insertChildQuestionNotice
// writes the parentheses frame with job_tool_call_id set. The UNBOUND
// batch-published variant is covered by the batch-question test in
// sessions_why_batch_question_test.go.
func TestExplainSessionStatus_AwaitingAnswerNamesQuestion(t *testing.T) {
	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "awaiting root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "awaiting-child", parent.ID, "worker")
	require.NoError(t, err)
	rootID, _ := seedCmdBatchDelegation(t, a.DB(), parent.ID, "awaiting-fixture-host", "delegate-9", child.ID)
	insertChildQuestionNotice(t, store, parent.ID, child.ID, "delegate-9", "adapter A or B?")

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	out := buf.String()

	require.Contains(t, out, "note: a held batch question is awaiting an answer",
		"the bound held question must be named as awaiting an answer")
	require.Contains(t, out, "waiting for your answer: child "+child.ID+" asked:",
		"the child that asked must be named")
	require.Contains(t, out, "adapter A or B?", "the question text must be quoted")
	require.Contains(t, out, "child_session_id=\""+child.ID+"\"",
		"the inject_agent answer path must name the child session")
	require.Contains(t, out, "Batch roots:")
	require.Contains(t, out, "delegate-9", "the held delegation's tool call must be named")
	require.Contains(t, out, rootID[:8], "the held root's short id must be named")
	require.Contains(t, out, "status: batch live",
		"the held delegation keeps the verdict live")
	require.NotContains(t, out, "status: at rest")
}

// TestExplainSessionStatus_NoQuestionNotice_StaysSilent: sessions without a
// pending child_question notice must not grow the awaiting-answer section,
// and a notice bound to ANOTHER owner's tool call must not leak in.
func TestExplainSessionStatus_NoQuestionNotice_StaysSilent(t *testing.T) {
	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "plain root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "plain-child", parent.ID, "worker")
	require.NoError(t, err)
	// The notice is bound to a DIFFERENT owner's tool call: the parent must
	// not see it.
	insertChildQuestionNotice(t, store, child.ID, child.ID, "delegate-x", "unrelated owner")

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	require.NotContains(t, buf.String(), "awaiting an answer")
	require.NotContains(t, buf.String(), "waiting for your answer")
}
