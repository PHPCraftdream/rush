package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// whyTestStore builds the hand-backed App explainWhy drives, plus the
// AsyncJobStore the awaiting-answer reader (#1158) goes through.
func whyAwaitingTestEnv(t *testing.T) (*app.App, *session.AsyncJobStore, string) {
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
	store := session.NewAsyncJobStore(conn, dataDir, 1, "why-test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a.SetAsyncJobStoreForTest(store)
	return a, store, dataDir
}

// insertChildQuestionNotice writes one child_question notice in the exact
// shape agent.childQuestionNoticeText produces (frame inside a preamble).
func insertChildQuestionNotice(t *testing.T, store *session.AsyncJobStore, owner, childID, jobID, question string) {
	t.Helper()
	text := "Supervision summary.\n\nSUB-AGENT QUESTION (session " + childID + "): QUESTION: " + question +
		"\n\nThe sub-agent is paused on this question.\nAnswer: call `agent` with resume_session_id=\"" + childID + "\"."
	require.NoError(t, store.InsertSessionNotice(context.Background(), owner, session.NoticeKindChildQuestion, text, true, jobID))
}

// TestExplainSessionStatus_AwaitingAnswerNamesChildAndAnswerPath: a pending
// child_question notice for a held delegation must surface as a dedicated
// "waiting for your answer" section naming the child, its question, the
// in-process answer path (agent + resume_session_id) and, with no live
// driver, the relay command for the operator (#1158).
func TestExplainSessionStatus_AwaitingAnswerNamesChildAndAnswerPath(t *testing.T) {
	t.Parallel()

	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "awaiting root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "awaiting-child", parent.ID, "worker")
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: parent.ID, ToolCallID: "delegate-9", Kind: session.JobKindAgent,
		Input: "delegate to " + child.ID, ChildSessionID: child.ID,
	})
	require.NoError(t, err)
	insertChildQuestionNotice(t, store, parent.ID, child.ID, "delegate-9", "adapter A or B?")

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	out := buf.String()

	require.Contains(t, out, "waiting for your answer: child "+child.ID+" asked:")
	require.Contains(t, out, "adapter A or B?")
	require.Contains(t, out, "resume_session_id=\""+child.ID+"\"")
	require.Contains(t, out, "delegation delegate-9")
	require.Contains(t, out, "rush sessions inject "+parent.ID,
		"no live driver: the operator must be told how to relay the question")
	require.Contains(t, out, "delegating",
		"the held delegation keeps the verdict delegating")
}

// TestExplainSessionStatus_AwaitingAnswer_WithLiveDriverOmitsRelay: when a
// live `rush run` driver exists, the answer belongs to the orchestrating
// agent; the operator relay line is omitted.
func TestExplainSessionStatus_AwaitingAnswer_WithLiveDriverOmitsRelay(t *testing.T) {
	t.Parallel()

	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "awaiting driven root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "awaiting-driven-child", parent.ID, "worker")
	require.NoError(t, err)
	insertChildQuestionNotice(t, store, parent.ID, child.ID, "delegate-7", "proceed?")
	require.NoError(t, store.ClaimSessionDriver(ctx, parent.ID))
	t.Cleanup(func() { _ = store.ReleaseSessionDriver(ctx, parent.ID) })

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	out := buf.String()

	require.Contains(t, out, "waiting for your answer: child "+child.ID)
	require.Contains(t, out, "resume_session_id=\""+child.ID+"\"")
	require.NotContains(t, out, "rush sessions inject",
		"a live driver answers in-process; no operator relay is needed")
	require.Contains(t, out, "driver:")
}

// TestExplainSessionStatus_NoQuestionNotice_StaysSilent: sessions without a
// pending child_question notice must not grow the section at all.
func TestExplainSessionStatus_NoQuestionNotice_StaysSilent(t *testing.T) {
	t.Parallel()

	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "plain root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "plain-child", parent.ID, "worker")
	require.NoError(t, err)
	insertChildQuestionNotice(t, store, child.ID, child.ID, "delegate-x", "unrelated owner")

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, parent.ID, &buf))
	require.NotContains(t, buf.String(), "waiting for your answer")
	require.False(t, strings.Contains(buf.String(), "rush sessions inject"))
}
