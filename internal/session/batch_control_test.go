package session

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestR9ControlNestedLifecycleTwoStores(t *testing.T) {
	for _, state := range []string{"pending", "cancelling"} {
		t.Run(state, func(t *testing.T) {
			a, b, conn, _, ctx := r9Stores(t)
			spec := batchStoreTestReservation()
			nested := spec.Root
			nested.Children = []BatchReservationNode{batchStoreTestTask(), batchStoreTestTask()}
			nested.Values.State = state
			spec.Root.Children = []BatchReservationNode{nested, batchStoreTestTask()}
			res, err := a.Reserve(ctx, spec)
			require.NoError(t, err)
			root := res.Snapshot.Root.RootID
			node := root + ".1.1"
			_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state=? WHERE node_id=?", state, node)
			require.NoError(t, err)
			before, err := a.Read(ctx, "owner", root)
			require.NoError(t, err)
			target := BatchControlTarget{"owner", "host", root, node}
			alive := func(string) (HostLockStatus, error) { return HostStatusAlive, nil }
			request, err := b.BatchRequestStop(ctx, target, alive)
			require.NoError(t, err)
			calls := 0
			pump := NewBatchControlPump(a, "host", func(ctx context.Context, r db.BatchControlRequest) (BatchControlAccepted, error) {
				calls++
				require.Equal(t, node, r.NodeID)
				require.Equal(t, root, r.RootID)
				// Callback uses the same writer: a held pump tx would deadlock here.
				update := batchStoreTestUpdate(node, "cancelling")
				update.Cause = "stop"
				e := a.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: root, Updates: []db.ProjectBatchNodeParams{update}}}})
				return BatchControlAccepted{Code: "accepted", Sequence: 2}, e
			})
			var wg sync.WaitGroup
			errs := make([]error, 4)
			counts := make([]int, 4)
			for i := range 4 {
				wg.Add(1)
				go func() { defer wg.Done(); counts[i], errs[i] = pump.Pump(ctx, "owner", root) }()
			}
			wg.Wait()
			total := 0
			for i := range 4 {
				require.NoError(t, errs[i])
				total += counts[i]
			}
			require.Equal(t, 1, total)
			require.Equal(t, 1, calls)
			answer, err := b.BatchWaitControl(ctx, target, request.RequestID)
			require.NoError(t, err)
			require.Equal(t, "answered", answer.Status)
			require.Equal(t, "accepted", answer.AnswerCode)
			require.EqualValues(t, 2, answer.AnswerSequence.Int64)
			after, err := b.Read(ctx, "owner", root)
			require.NoError(t, err)
			for i, n := range after.Nodes {
				if n.NodeID == root || n.NodeID == node {
					continue
				}
				require.Equal(t, before.Nodes[i], n)
			}
			for _, n := range after.Nodes {
				require.True(t, batchStateLive(n.State))
				require.Zero(t, n.NoticeSelected)
			}
			replacement := NewBatchControlPump(b, "host", func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error) {
				t.Fatal("answered redelivery")
				return BatchControlAccepted{}, nil
			})
			count, err := replacement.Pump(ctx, "owner", root)
			require.NoError(t, err)
			require.Zero(t, count)
		})
	}
}

func TestR9ControlRejectionsNoPersistence(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	snap := batchStoreTestReserve(t, a, ctx)
	target := BatchControlTarget{"owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID}
	alive := func(string) (HostLockStatus, error) { return HostStatusAlive, nil }
	for _, tc := range []struct {
		name   string
		target BatchControlTarget
		want   error
	}{
		{"stale", BatchControlTarget{"owner", "host", "old", target.Node}, ErrBatchControlStale},
		{"wrong-owner", BatchControlTarget{"other", "host", target.Root, target.Node}, ErrBatchControlStale},
		{"wrong-host", BatchControlTarget{"owner", "foreign", target.Root, target.Node}, ErrBatchControlHostMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := a.BatchRequestStop(ctx, tc.target, alive)
			require.ErrorIs(t, e, tc.want)
			require.Empty(t, r.RequestID)
		})
	}
	_, err := a.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusUnknown, nil })
	require.ErrorIs(t, err, ErrBatchControlUnknownHost)
	_, err = a.BatchRequestStop(ctx, target, BatchSharedHostProbe(dir))
	require.ErrorIs(t, err, ErrBatchControlStale)
	// Unannounced dead root was deleted, not callback-cancelled.
	_, err = a.Read(ctx, "owner", target.Root)
	require.ErrorIs(t, err, sql.ErrNoRows)
	snap = batchStoreTestReserve(t, a, ctx)
	target.Root, target.Node = snap.Root.RootID, snap.Nodes[1].NodeID
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state='completed' WHERE node_id=?", target.Node)
	require.NoError(t, err)
	_, err = a.BatchRequestStop(ctx, target, alive)
	require.ErrorIs(t, err, ErrBatchControlTerminal)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_control_requests").Scan(&count))
	require.Zero(t, count)
	pump := NewBatchControlPump(a, "foreign", func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error) {
		t.Fatal("nonowner callback")
		return BatchControlAccepted{}, nil
	})
	_, err = pump.Pump(ctx, "owner", target.Root)
	require.ErrorIs(t, err, ErrBatchControlHostMismatch)
}

func TestR9ControlWaitTimeoutAndCommitProof(t *testing.T) {
	a, b, _, _, ctx := r9Stores(t)
	snap := batchStoreTestReserve(t, a, ctx)
	target := BatchControlTarget{"owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID}
	r, err := b.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusAlive, nil })
	require.NoError(t, err)
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = b.BatchWaitControl(wait, target, r.RequestID)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	pump := NewBatchControlPump(a, "host", func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error) {
		return BatchControlAccepted{Code: "accepted"}, nil
	})
	_, err = pump.Pump(ctx, "owner", target.Root)
	require.ErrorIs(t, err, ErrBatchProjectionConflict)
	got, err := a.BatchReadControl(ctx, target, r.RequestID)
	require.NoError(t, err)
	require.Equal(t, r, got)
}
