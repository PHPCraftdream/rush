package server

// R2C-13: the web session list asks "does a descendant / the session itself
// have live work" for EVERY listed session on each 5s re-poll. Through the
// real handler, over a forest (nested delegation, an own plain job, a live
// driver between turns, many idle sessions), the annotations must equal
// what the single classifier reader says for each session.
//
// All live-work facts are seeded as live batch roots through the production
// session.BatchStore.Reserve API (seedLiveWorkRow, livework_seed_test.go):
// delegation roots carry kind=batch with an agent task child naming the
// delegation child session; plain jobs are bare bash roots.
//
// Final-world note: the legacy dead-host scenario this test used to carry
// (raw async_jobs rows on a provably dead host reading as NOT live work) is
// gone — under the batch authority dead hosts stay live until recovery
// commits (internal/app/app_batch_cutover_facts.go), so there is no
// dead-host-running-rows fact to assert. The countingReadDBTX /
// WrapReadQueriesForTest reader instrumentation went with the
// AsyncJobStore; the derived-relation (bounded-reads) property for the
// annotation path is pinned by descendant_work_annotation_reads_on_test.go.

import (
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHandleListSessions_LiveWorkAnnotationIsBatched(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	// root-a -> mid-a -> leaf-a (nested delegation); leaf-a runs a plain
	// job of its own.
	rootA, err := a.Sessions.Create(ctx, "root a")
	require.NoError(t, err)
	midA, err := a.Sessions.CreateTaskSession(ctx, "batch-mid-a", rootA.ID, "mid a")
	require.NoError(t, err)
	leafA, err := a.Sessions.CreateTaskSession(ctx, "batch-leaf-a", midA.ID, "leaf a")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: rootA.ID, toolCallID: "d1", taskKind: "agent", childSessionID: midA.ID, state: "running"})
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: midA.ID, toolCallID: "d2", taskKind: "agent", childSessionID: leafA.ID, state: "running"})
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: leafA.ID, toolCallID: "p1", taskKind: "bash", state: "running"})

	// root-own: its own plain job, nothing below it.
	rootOwn, err := a.Sessions.Create(ctx, "root own")
	require.NoError(t, err)
	seedLiveWorkRow(t, a, liveWorkSeedSpec{owner: rootOwn.ID, toolCallID: "p2", taskKind: "bash", state: "running"})

	// driven: a live `rush run` loop between turns -- no lock, no running
	// row; the driver marker is the only live fact.
	driven, err := a.Sessions.Create(ctx, "driven between turns")
	require.NoError(t, err)
	hostStore := a.BatchHostStore()
	require.NotNil(t, hostStore, "a full App built via appPkg.New has a batch host store")
	require.NoError(t, hostStore.ClaimSessionDriver(ctx, driven.ID))

	addIdle := func(n int) {
		for i := 0; i < n; i++ {
			_, err := a.Sessions.Create(ctx, fmt.Sprintf("idle %d", i))
			require.NoError(t, err)
		}
	}
	addIdle(6)

	hub := newHub()
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-hubDone })
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client
	list := func() []session.Session {
		handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
		replies := drainSessionsListReplies(t, client)
		require.NotEmpty(t, replies)
		return replies[len(replies)-1]
	}

	rows := list()
	require.Len(t, rows, 9, "3 roots with work + 6 idle top-level sessions")

	a1 := findSessionRow(t, rows, rootA.ID)
	require.True(t, a1.HasLiveDescendantWork)
	require.Equal(t, []string{midA.ID, leafA.ID}, a1.LiveDescendantIDs, "nested delegation: every live descendant, once")
	// By-design final-world semantics: the depth-0 live batch root is the
	// owner's OWN work too (BatchLiveForSessions counts it in Own), so the
	// delegating root also flags HasLiveOwnWork — pinned by
	// handlers_sessions_batch_cutover_test.go.
	require.True(t, a1.HasLiveOwnWork)
	own := findSessionRow(t, rows, rootOwn.ID)
	require.True(t, own.HasLiveOwnWork)
	require.False(t, own.HasLiveDescendantWork)
	require.Empty(t, own.LiveDescendantIDs)
	drv := findSessionRow(t, rows, driven.ID)
	require.True(t, drv.HasLiveOwnWork, "a live driver marker between turns is live own work")
	require.False(t, drv.HasLiveDescendantWork)
	require.Empty(t, drv.LiveDescendantIDs)

	// The annotation is exactly what the single classifier reader's
	// facts-based mapping says for each row: a live driver between turns
	// flags HasLiveOwnWork but contributes no descendant ids.
	for _, s := range rows {
		act, err := a.SessionActivity(ctx, s.ID)
		require.NoErrorf(t, err, "single reader of %s", s.ID)
		f := act.Facts
		require.Equalf(t, f.Driver != nil || f.OwnRunningJobs > 0, s.HasLiveOwnWork, "own flag vs classifier facts of %s", s.ID)
		require.Equalf(t, f.LiveDelegations > 0, s.HasLiveDescendantWork, "descendant flag vs classifier facts of %s", s.ID)
		require.Equalf(t, f.LiveDescendantSessionIDs, s.LiveDescendantIDs, "descendant ids vs classifier facts of %s", s.ID)
	}

	addIdle(60)
	rows = list()
	require.Len(t, rows, 69)
	// Ten times the sessions: the same annotations hold at scale.
	for _, s := range rows {
		if s.ID != rootA.ID {
			continue
		}
		require.True(t, s.HasLiveDescendantWork, "the nested delegation is still live with 60 more idle sessions")
	}
}
