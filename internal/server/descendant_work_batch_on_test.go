// R12-C instrumentation, flag ON: the live-work panel's batch snapshot must
// keep reading the delegation forest through the batchSourceFor seam in a
// BFS-chunked way (about one BatchLiveNodesForOwners chunk per tree depth,
// never one query per session), and the snapshot payload must actually carry
// the reserved batch roots with their delegation child links. The legacy
// test (descendant_work_batch_test.go) keeps its flag-OFF assertions
// untouched; this file adds the flag-ON half.
//
// Mutations this test catches:
//   - a per-session query loop in the batch live walk → 31+ reads for 31
//     delegation children, failing the ≤5 query-count assertions;
//   - silently dropped instrumentation (the counting wrapper never installed,
//     or the reads bypassing the seam) → the >0 precondition fails at 0;
//   - losing the delegation child links (ChildSessionID) → the list
//     annotation assertions and the payload's ChildSessionID fail.
//
// Seam disclosure: the LIST annotation handler's flag-ON batch facts come
// from App.SessionActivityBatch (handlers_sessions.go →
// internal/app/app_session_activity.go → batchCutoverLiveWork), which has NO
// injectable seam in this package — the annotation assertions below are
// therefore UNINSTRUMENTED. The countable instrumented surface here is the
// flat batch snapshot reader (buildSessionLiveWorkFromBatch via the
// batchSourceFor seam). batchPendingQuestions reads session_notices through
// db.New(a.DB()) directly, not through the seam, so it is not counted
// either.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// countingBatchSource delegates every read to the real production source and
// counts ONLY the LIVE reads (BatchLiveNodesForOwners and BatchLevelNodes) —
// the R12-C terminal-root history read (BatchHistoryRootsForOwner) is a
// different fact kind and deliberately not counted.
type countingBatchSource struct {
	inner   batchNodeSource
	queries *atomic.Int64
}

func (c countingBatchSource) BatchLiveNodesForOwners(ctx context.Context, owners []string) ([]db.BatchNode, error) {
	c.queries.Add(1)
	return c.inner.BatchLiveNodesForOwners(ctx, owners)
}

func (c countingBatchSource) BatchHistoryRootsForOwner(ctx context.Context, owner string) ([]db.BatchNode, error) {
	return c.inner.BatchHistoryRootsForOwner(ctx, owner)
}

func (c countingBatchSource) BatchLevelNodes(ctx context.Context, p db.BatchLevelNodesParams) ([]db.BatchNode, error) {
	c.queries.Add(1)
	return c.inner.BatchLevelNodes(ctx, p)
}

// reserveLiveDelegationRoot reserves one live batch root owned by owner with
// one running agent task child whose ChildSessionID names the delegation
// child session — the same production BatchStore.Reserve shape
// sessions_why_descendant_test.go's claimDelegation uses in its flag-ON
// branch, with the root held nonterminal ("running") so the live walk keeps
// it.
func reserveLiveDelegationRoot(t *testing.T, ctx context.Context, a *app.App, host, owner, toolCallID, childID string) {
	t.Helper()
	_, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "on-test-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: "agent", Valid: true},
				State: "running", ControlState: "none",
				ChildSessionID: sql.NullString{String: childID, Valid: true},
			}},
		}},
	})
	require.NoError(t, err)
}

func TestHandleGetSessionLiveWork_BatchOnInstrumentedChunkedRead(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, batchRuntimeEnabled())
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	// The own host: reserved roots on it are alive to the reader without any
	// probing gymnastics (the own-host registry is alive by definition).
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)

	parent, err := a.Sessions.Create(ctx, "on parent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "on-child-0", parent.ID, "first delegation child")
	require.NoError(t, err)
	reserveLiveDelegationRoot(t, ctx, a, host, parent.ID, "on-tc-parent", child.ID)

	// 30 further delegation children, each with its own live root, so the
	// read-layer bound below has teeth (31 owners at depth 1).
	for i := 1; i <= 30; i++ {
		ci, err := a.Sessions.CreateTaskSession(ctx, fmt.Sprintf("on-child-%d", i), parent.ID, "delegation child")
		require.NoError(t, err)
		reserveLiveDelegationRoot(t, ctx, a, host, parent.ID, fmt.Sprintf("on-tc-%d", i), ci.ID)
	}

	// List annotations through the real handler (uninstrumented: these facts
	// come from App.SessionActivityBatch, which has no seam here).
	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client
	handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
	replies := drainSessionsListReplies(t, client)
	require.NotEmpty(t, replies)
	rows := replies[len(replies)-1]
	parentRow := findSessionRow(t, rows, parent.ID)
	require.True(t, parentRow.HasLiveDescendantWork, "the live batch delegation is descendant work")
	// The fixture seeds 31 delegation children, so the annotation names all
	// of them (walk order, deduped) — contrast with
	// handlers_sessions_batch_cutover_test.go:113, which asserts the exact
	// single-element slice because it seeds one delegation.
	require.Len(t, parentRow.LiveDescendantIDs, 31, "every live delegation child the walk reached, deduped")
	require.Contains(t, parentRow.LiveDescendantIDs, child.ID, "the first seeded delegation child is named")
	// By-design flag-ON divergence from the legacy test's false: every
	// depth-0 live batch root is the OWNER'S OWN work
	// (internal/session/batch_read_live.go:133-138 →
	// internal/app/app_batch_cutover_facts.go:86-90 →
	// internal/server/handlers_sessions.go:276), pinned TRUE by
	// handlers_sessions_batch_cutover_test.go:112-117.
	require.True(t, parentRow.HasLiveOwnWork, "the depth-0 live batch root is the session's own live work")

	// Instrument ONLY the flat snapshot reader: swap the batchSourceFor seam
	// (never withBatchSeams — that would also fake the probe/dir/own-host
	// seams, and the real ones are part of what is exercised here).
	realSrc := batchSourceFor(a)
	var queries atomic.Int64
	origSourceFor := batchSourceFor
	batchSourceFor = func(*app.App) batchNodeSource {
		return countingBatchSource{inner: realSrc, queries: &queries}
	}
	t.Cleanup(func() { batchSourceFor = origSourceFor })

	// ONE snapshot of the parent, counter reset right before so the
	// precondition and the bound are about THIS snapshot.
	queries.Store(0)
	snap := requestSnapshot(t, a, client, parent.ID)
	require.NotNil(t, snap)
	require.Greater(t, queries.Load(), int64(0),
		"precondition: the wrapped source must have served the reads — zero means the instrumentation is broken")
	// Depth 0 chunk (1 owner: the parent) + depth 1 chunk (31 owners, far
	// below the 500 chunk size of batchCollectRows, handlers_batch.go:207-244)
	// = 2 expected live reads; 5 is the generous bound. A per-session query
	// loop would need 31+ reads and fails here.
	require.LessOrEqual(t, queries.Load(), int64(5),
		"the batch live walk is BFS-chunked: ~1 chunk per depth, never one query per session")

	// The payload is populated: 31 agent roots (one per delegation child),
	// the known root present with its delegation child link.
	require.Len(t, snap.Agents, 31, "one Agents item per live root, task_kind=agent")
	var first LiveWorkItemWire
	found := false
	for _, it := range snap.Agents {
		if it.ToolCallID == "on-tc-parent" {
			first, found = it, true
			break
		}
	}
	require.True(t, found, "the seeded parent root is in the snapshot")
	require.Equal(t, "running", first.Status)
	require.Equal(t, "agent", first.ToolName)
	require.Equal(t, child.ID, first.ChildSessionID, "the delegation child link must survive the read")

	// N-scaling teeth: a second poll of the same parent must cost the same
	// chunked reads — the count must not grow per descendant across polls.
	queries.Store(0)
	snap2 := requestSnapshot(t, a, client, parent.ID)
	require.NotNil(t, snap2)
	require.LessOrEqual(t, queries.Load(), int64(5),
		"repeated polls stay chunked: the read count does not grow per descendant")
}
