package session

// PD11B change (a): read-pool routing of pure batch reads. Read must run its
// read-only tx on the optional reader pool, never on the single-connection
// writer; nil/identical readers keep the pre-PD11B writer-routed behavior,
// and a WAL reader observes committed writes (read-your-committed-write).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// batchReaderTestDBs opens a writer pool and a WAL read pool on the same
// fresh database and releases BOTH via t.Cleanup (db.ReleaseConn).
func batchReaderTestDBs(t *testing.T) (ctx context.Context, w, r *sql.DB) {
	t.Helper()
	ctx = t.Context()
	dataDir := t.TempDir()
	var err error
	w, err = db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(w)) })
	r, err = db.ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(r)) })
	// batch_nodes.owner_session_id references sessions(id); seed the owner
	// through the writer like the other BatchStore fixtures.
	require.NoError(t, seedSession(ctx, db.New(w), "owner"))
	return ctx, w, r
}

func TestBatchStoreReadThroughReader(t *testing.T) {
	ctx, w, r := batchReaderTestDBs(t)
	if r == w {
		t.Skip("degraded mode: read pool is the writer")
	}
	storeW := NewBatchStore(w)
	storeR := NewBatchStoreWithReader(w, r)
	snap := batchStoreTestReserve(t, storeW, ctx)
	root := snap.Root.RootID

	// Hold the writer's ONLY connection with an open tx (trivial query
	// first so the connection is really checked out). If Read regressed to
	// the writer pool, its BeginTx would block behind this tx and the 10s
	// timeout below would fire.
	holdTx, err := w.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holdTx.Rollback() }()
	var one int
	require.NoError(t, holdTx.QueryRowContext(ctx, "SELECT 1").Scan(&one))

	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type readResult struct {
		snap BatchSnapshot
		err  error
	}
	ch := make(chan readResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, e := storeR.Read(readCtx, "owner", root)
		ch <- readResult{snap: s, err: e}
	}()
	// Join the reader goroutine on every exit path (the deferred cancel and
	// rollback above run first, which unblocks it on the failure branch).
	t.Cleanup(func() { <-done })
	select {
	case res := <-ch:
		require.NoError(t, res.err)
		require.Equal(t, root, res.snap.Root.RootID)
		require.Len(t, res.snap.Nodes, 3)
	case <-readCtx.Done():
		t.Fatalf("storeR.Read blocked: read tx did not run on the reader pool")
	}
}

func TestBatchStoreReaderReadYourCommittedWrite(t *testing.T) {
	ctx, w, r := batchReaderTestDBs(t)
	if r == w {
		t.Skip("degraded mode: read pool is the writer")
	}
	storeW := NewBatchStore(w)
	storeR := NewBatchStoreWithReader(w, r)
	snap := batchStoreTestReserve(t, storeW, ctx)
	root := snap.Root.RootID

	got, err := storeR.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, root, got.Root.RootID)
	require.Len(t, got.Nodes, 3)

	// Terminal projection through the writer (fences host/runner via
	// AdvanceBatchRootSequence — same shape as batch_stop_fence_test.go).
	p := BatchProjection{Host: snap.Root.HostID, RunnerID: snap.Root.RunnerID, Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: root, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, "completed")}}}}
	require.NoError(t, storeW.Project(ctx, p))

	after, err := storeR.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, "completed", after.Root.State)
}

func TestBatchStoreReaderParityWithWriterStore(t *testing.T) {
	ctx, w, r := batchReaderTestDBs(t)
	storeW := NewBatchStore(w)
	snap := batchStoreTestReserve(t, storeW, ctx)
	root := snap.Root.RootID

	viaReader, err := NewBatchStoreWithReader(w, r).Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, snap, viaReader)

	// Degraded variants share the writer; only functional parity here (no
	// hold tx — they legitimately need the writer connection).
	viaNil, err := NewBatchStore(w).Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, snap, viaNil)
	viaSame, err := NewBatchStoreWithReader(w, w).Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, snap, viaSame)
}
