package server

// R2C-13: the web session list asks "does a descendant / the session itself
// have live work" for EVERY listed session on each 5s re-poll. Through the
// real handler, over a forest (nested delegation, a dead host, an own plain
// job, many idle sessions), the annotations must equal what the single-root
// readers say for each session while the reader queries depend on the tree
// depth, not on the number of sessions.

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// countingReadDBTX counts the reader queries the async job store issues.
type countingReadDBTX struct {
	db.DBTX
	n *atomic.Int64
}

func (c countingReadDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	c.n.Add(1)
	return c.DBTX.QueryContext(ctx, query, args...)
}

func (c countingReadDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	c.n.Add(1)
	return c.DBTX.QueryRowContext(ctx, query, args...)
}

// Revert-check performed: annotateLiveWork reimplemented as the previous
// per-session loop over LiveDescendantJobs/LiveOwnJobs -- the annotation
// assertions still passed but the query-count assertion FAILED (20 reader
// queries for 9 listed sessions instead of at most 4).
func TestHandleListSessions_LiveWorkAnnotationIsBatched(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	dataDir := a.Config().Options.DataDirectory

	store := a.AsyncJobStore()
	require.NotNil(t, store)

	// root-a -> mid-a -> leaf-a (nested delegation on the own host); leaf-a
	// runs a plain job of its own.
	rootA, err := a.Sessions.Create(ctx, "root a")
	require.NoError(t, err)
	midA, err := a.Sessions.CreateTaskSession(ctx, "batch-mid-a", rootA.ID, "mid a")
	require.NoError(t, err)
	leafA, err := a.Sessions.CreateTaskSession(ctx, "batch-leaf-a", midA.ID, "leaf a")
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: rootA.ID, ToolCallID: "d1", Kind: session.JobKindAgent, Input: "a", ChildSessionID: midA.ID})
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: midA.ID, ToolCallID: "d2", Kind: session.JobKindAgent, Input: "b", ChildSessionID: leafA.ID})
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: leafA.ID, ToolCallID: "p1", Kind: session.JobKindCommand, Input: "sleep", ToolName: "bash"})
	require.NoError(t, err)

	// root-own: its own plain job, nothing below it.
	rootOwn, err := a.Sessions.Create(ctx, "root own")
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: rootOwn.ID, ToolCallID: "p2", Kind: session.JobKindCommand, Input: "sleep", ToolName: "bash"})
	require.NoError(t, err)

	// root-dead: a delegation and a plain job on a host whose lock nobody
	// holds -- provably dead, so neither counts as live work.
	rootDead, err := a.Sessions.Create(ctx, "root dead")
	require.NoError(t, err)
	deadChild, err := a.Sessions.CreateTaskSession(ctx, "batch-dead-child", rootDead.ID, "dead child")
	require.NoError(t, err)
	deadSeed, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "batch-dead-host"))
	require.NoError(t, err)
	require.NoError(t, deadSeed.Release())
	q := db.New(a.DB())
	for _, row := range []db.ClaimAsyncJobParams{
		{OwnerSessionID: rootDead.ID, ToolCallID: "dd1", Kind: "agent", ToolName: "agent", InputHash: "h1", HostID: "batch-dead-host",
			ChildSessionID: sql.NullString{String: deadChild.ID, Valid: true}, CreatedAt: 1700000000, UpdatedAt: 1700000000},
		{OwnerSessionID: rootDead.ID, ToolCallID: "dp1", Kind: "command", ToolName: "bash", InputHash: "h2", HostID: "batch-dead-host",
			CreatedAt: 1700000000, UpdatedAt: 1700000000},
	} {
		_, err = q.ClaimAsyncJob(ctx, row)
		require.NoError(t, err)
	}

	addIdle := func(n int) {
		for i := 0; i < n; i++ {
			_, err := a.Sessions.Create(ctx, fmt.Sprintf("idle %d", i))
			require.NoError(t, err)
		}
	}
	addIdle(6)

	var queries atomic.Int64
	store.WrapReadQueriesForTest(func(d db.DBTX) db.DBTX { return countingReadDBTX{DBTX: d, n: &queries} })

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client
	list := func() ([]session.Session, int64) {
		queries.Store(0)
		handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
		n := queries.Load()
		replies := drainSessionsListReplies(t, client)
		require.NotEmpty(t, replies)
		return replies[len(replies)-1], n
	}

	rows, smallQueries := list()
	require.Len(t, rows, 9, "3 roots with work + 6 idle top-level sessions")

	a1 := findSessionRow(t, rows, rootA.ID)
	require.True(t, a1.HasLiveDescendantWork)
	require.Equal(t, []string{midA.ID, leafA.ID}, a1.LiveDescendantIDs, "nested delegation: every live descendant, once")
	require.False(t, a1.HasLiveOwnWork)
	own := findSessionRow(t, rows, rootOwn.ID)
	require.True(t, own.HasLiveOwnWork)
	require.False(t, own.HasLiveDescendantWork)
	require.Empty(t, own.LiveDescendantIDs)
	dead := findSessionRow(t, rows, rootDead.ID)
	require.False(t, dead.HasLiveDescendantWork, "a delegation on a dead host is not live work")
	require.False(t, dead.HasLiveOwnWork, "a plain job on a dead host is not live work")

	// The annotation is exactly what the single-root readers say for each row.
	for _, s := range rows {
		wantDesc, _ := store.LiveDescendantJobs(ctx, s.ID)
		wantOwn, _ := store.LiveOwnJobs(ctx, s.ID)
		require.Equalf(t, len(wantDesc) > 0, s.HasLiveDescendantWork, "descendant flag of %s", s.ID)
		require.Equalf(t, len(wantOwn) > 0, s.HasLiveOwnWork, "own flag of %s", s.ID)
	}

	// Depth of the tree is 3 (root-a -> mid-a -> leaf-a): one query per level.
	require.LessOrEqual(t, smallQueries, int64(4), "reader queries must depend on the tree depth")

	addIdle(60)
	rows, largeQueries := list()
	require.Len(t, rows, 69)
	require.Equal(t, smallQueries, largeQueries, "ten times the sessions must cost the same number of reader queries")
}
