package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchCLIFindExactRootWinsOverChildPrefix(t *testing.T) {
	store, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, store, ctx)
	identity, node, err := store.BatchCLIFind(ctx, snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, snap.Root.RootID, node)
	require.Equal(t, "owner", identity.Owner)
}

func TestBatchCLIFindRootPrefixAndExactNestedNode(t *testing.T) {
	store, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, store, ctx)
	identity, node, err := store.BatchCLIFind(ctx, snap.Root.RootID[:8])
	require.NoError(t, err)
	require.Equal(t, snap.Root.RootID, node)
	require.Equal(t, "owner", identity.Owner)

	nested := snap.Nodes[1].NodeID
	identity, node, err = store.BatchCLIFind(ctx, nested)
	require.NoError(t, err)
	require.Equal(t, nested, node)
	require.Equal(t, snap.Root.RootID, identity.Root)
}

func TestBatchCLIFindMissing(t *testing.T) {
	store, _, ctx := batchStoreTestDB(t)
	_, _, err := store.BatchCLIFind(ctx, "does-not-exist")
	require.Error(t, err)
}

func TestBatchCLIUnknownHostStopRequestWritesNothing(t *testing.T) {
	store, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, store, ctx)
	target := BatchControlTarget{Owner: "owner", Host: "host", Root: snap.Root.RootID, Node: snap.Root.RootID}
	_, err := store.BatchRequestStop(ctx, target, func(string) (HostLockStatus, error) { return HostStatusUnknown, nil })
	require.ErrorIs(t, err, ErrBatchControlUnknownHost)
	var count int
	require.NoError(t, store.writer.QueryRowContext(ctx, `SELECT count(*) FROM batch_control_requests`).Scan(&count))
	require.Zero(t, count)
}
