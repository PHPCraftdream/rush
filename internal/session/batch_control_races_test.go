package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestR9ControlTerminalRaces(t *testing.T) {
	for _, mode := range []string{"node", "root"} {
		t.Run(mode, func(t *testing.T) {
			a, b, _, _, ctx := r9Stores(t)
			snap := batchStoreTestReserve(t, a, ctx)
			target := BatchControlTarget{"owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID}
			r, err := b.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusAlive, nil })
			require.NoError(t, err)
			id := target.Node
			code := "terminal"
			rejection := ErrBatchControlTerminal
			if mode == "root" {
				id = target.Root
				code = "stale"
				rejection = ErrBatchControlStale
			}
			require.NoError(t, a.Project(ctx, BatchProjection{Host: "host", Sequence: 9, Roots: []BatchRootProjection{{Owner: "owner", Root: target.Root, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(id, "completed")}}}}))
			before, err := a.Read(ctx, "owner", target.Root)
			require.NoError(t, err)
			_, err = b.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusAlive, nil })
			require.ErrorIs(t, err, rejection)
			pump := NewBatchControlPump(a, "host", func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error) {
				t.Fatal("terminal callback")
				return BatchControlAccepted{}, nil
			})
			count, err := pump.Pump(ctx, "owner", target.Root)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			answer, err := b.BatchReadControl(ctx, target, r.RequestID)
			require.NoError(t, err)
			require.Equal(t, code, answer.AnswerCode)
			require.EqualValues(t, 9, answer.AnswerSequence.Int64)
			count, err = pump.Pump(ctx, "owner", target.Root)
			require.NoError(t, err)
			require.Zero(t, count)
			after, err := a.Read(ctx, "owner", target.Root)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestR9UnknownNilNoWrites(t *testing.T) {
	a, b, _, _, ctx := r9Stores(t)
	snap := batchStoreTestReserve(t, a, ctx)
	target := BatchControlTarget{"owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID}
	unknown := func(string) (HostLockStatus, error) { return HostStatusUnknown, nil }
	out, err := a.BatchRecoverDeadHost(ctx, "host", unknown)
	require.NoError(t, err)
	require.Equal(t, BatchRecoveryOutcome{}, out)
	_, err = b.BatchRequestStop(ctx, target, unknown)
	require.ErrorIs(t, err, ErrBatchControlUnknownHost)
	got, err := a.Read(ctx, "owner", target.Root)
	require.NoError(t, err)
	require.Equal(t, snap, got)
	pending, err := a.PendingControls(ctx, "owner", "host", target.Root)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestR9DeadTerminalPendingControls(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "terminal"}[terminal], func(t *testing.T) {
			a, b, conn, dir, ctx := r9Stores(t)
			snap := batchStoreTestReserve(t, a, ctx)
			r9Announce(t, conn, ctx, snap.Root.RootID)
			target := BatchControlTarget{"owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID}
			r, err := b.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusAlive, nil })
			require.NoError(t, err)
			if terminal {
				_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state='completed',updated_at=1")
				require.NoError(t, err)
			}
			before, err := a.Read(ctx, "owner", target.Root)
			require.NoError(t, err)
			count, err := a.BatchCountExpired(ctx, 100)
			require.NoError(t, err)
			require.Zero(t, count)
			purged, err := a.BatchPurgeExpired(ctx, 100)
			require.NoError(t, err)
			require.Zero(t, purged)
			out, err := a.BatchSweepDeadHosts(ctx, "", BatchSharedHostProbe(dir))
			require.NoError(t, err)
			require.EqualValues(t, 1, out["host"].Controls)
			answer, err := b.BatchReadControl(ctx, target, r.RequestID)
			require.NoError(t, err)
			require.Equal(t, "answered", answer.Status)
			require.Positive(t, answer.AnswerSequence.Int64)
			if terminal {
				require.Equal(t, "stale", answer.AnswerCode)
				after, e := a.Read(ctx, "owner", target.Root)
				require.NoError(t, e)
				require.Equal(t, before, after)
			} else {
				require.Equal(t, "host_lost", answer.AnswerCode)
			}
			again, err := a.BatchSweepDeadHosts(ctx, "", BatchSharedHostProbe(dir))
			require.NoError(t, err)
			require.Empty(t, again)
		})
	}
}
