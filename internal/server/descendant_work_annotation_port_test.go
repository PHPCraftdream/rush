package server

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func testHandleListSessionsLiveWorkAnnotationBatchOn(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	_, err := db.Connect(ctx, dataDir)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Release(dataDir) })
	c := &annotationCounters{}
	pool := annotationCountingPool(t, c, dataDir)
	s := session.NewService(db.New(pool), pool)
	a := &app.App{Sessions: s, DB: func() *sql.DB { return pool }}
	a.SetDataDirForTest(dataDir)
	hubCtx, cancel := context.WithCancel(ctx)
	hub := newHub()
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(hubCtx) }()
	t.Cleanup(func() { cancel(); <-hubDone })
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client

	addOwner := func(name string, child bool) (string, string) {
		owner, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		childID := ""
		if child {
			descendant, err := a.Sessions.CreateTaskSession(ctx, name+"-child", owner.ID, "delegation child")
			require.NoError(t, err)
			childID = descendant.ID
		}
		reserveAnnotationRoot(t, ctx, pool, owner.ID, "tc-"+name, childID)
		return owner.ID, childID
	}
	firstOwner, firstChild := addOwner("descendant-owner-0", true)
	addOwner("descendant-owner-1", true)
	addOwner("descendant-owner-2", false)
	for i := 3; i < 12; i++ {
		addOwner(fmt.Sprintf("descendant-owner-%d", i), false)
	}

	c.stmts.Store(0)
	c.txs.Store(0)
	handleListSessions(ctx, a, client, WSMessage{ID: "req-on", Type: CmdListSessions})
	replies := drainSessionsListReplies(t, client)
	require.NotEmpty(t, replies)
	rows := replies[len(replies)-1]
	require.Len(t, rows, 12)
	require.Greater(t, c.stmts.Load(), int64(0), "counting pool must observe annotation reads")
	require.Equal(t, int64(5+4*12), c.stmts.Load(), "5 fixed statements plus 4 per top-level owner")
	require.Equal(t, int64(2), c.txs.Load(), "one shared debt read and one live BFS read transaction")
	row := findSessionRow(t, rows, firstOwner)
	require.True(t, row.HasLiveOwnWork)
	require.True(t, row.HasLiveDescendantWork)
	require.Equal(t, []string{firstChild}, row.LiveDescendantIDs)
}
