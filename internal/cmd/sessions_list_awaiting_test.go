package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestListStatusesWithAwaiting_AnnotatesAndWires: the list helper must (a)
// re-classify a live verdict from the root's pending child_question notices
// into the "(awaiting answer)" STATUS annotation and (b) expose the child +
// question for the --json wire fields (#1158). Revert-check: removing the
// notice read or the enrichment in listStatusesWithAwaiting leaves the
// plain "delegating" status and an empty awaiting map, failing both
// assertions.
func TestListStatusesWithAwaiting_AnnotatesAndWires(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	s := session.NewService(q, conn)
	parent, err := s.Create(ctx, "awaiting list root")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(ctx, "awaiting-list-child", parent.ID, "worker")
	require.NoError(t, err)

	store := session.NewAsyncJobStore(conn, dataDir, 1, "list-test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: parent.ID, ToolCallID: "delegate-3", Kind: session.JobKindAgent,
		Input: "delegate to " + child.ID, ChildSessionID: child.ID,
	})
	require.NoError(t, err)
	insertChildQuestionNotice(t, store, parent.ID, child.ID, "delegate-3", "ship or hold?")

	a := &app.App{Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetAsyncJobStoreForTest(store)
	acts, err := a.SessionActivityBatch(ctx, []string{parent.ID})
	require.NoError(t, err)

	statusByID, awaitingByID := listStatusesWithAwaiting(ctx, store, acts, []string{parent.ID})
	require.Contains(t, statusByID[parent.ID], "awaiting answer",
		"a held delegation with a pending child question must be annotated")
	require.Contains(t, statusByID[parent.ID], "delegating")
	got := awaitingByID[parent.ID]
	require.Equal(t, child.ID, got.ChildSessionID)
	require.Equal(t, "delegate-3", got.DelegationToolCallID)
	require.Contains(t, got.Question, "ship or hold?")
}

// TestListStatusesWithAwaiting_NilStoreIsPlainStatus: no store (a
// configless App) keeps the plain verdicts and no awaiting entries.
func TestListStatusesWithAwaiting_NilStoreIsPlainStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	s := session.NewService(q, conn)
	parent, err := s.Create(ctx, "plain list root")
	require.NoError(t, err)
	a := &app.App{Sessions: s, DB: func() *sql.DB { return conn }}
	acts, err := a.SessionActivityBatch(ctx, []string{parent.ID})
	require.NoError(t, err)

	statusByID, awaitingByID := listStatusesWithAwaiting(ctx, nil, acts, []string{parent.ID})
	require.NotContains(t, statusByID[parent.ID], "awaiting answer")
	require.Empty(t, awaitingByID)
}
