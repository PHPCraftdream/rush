package session

// DISPOSITION (P4 fix round 1, final one-migration world): the old
// TestBatchMigrationUpgradeAndDownPreservesRows exercised a multi-migration
// FS (three excluded batch filenames) and three provider.Down calls. In the
// final world there is ONE embedded initial migration and db.Connect applies
// the whole of db.FS, so the FS filter and the selective Down ladder are
// obsolete and were deleted. The down/up "batch tables drop, retained tables
// survive" round trip was dropped too: the squashed single migration
// (internal/db/testdata/store_v1/20261015000001_store_v1_initial.sql) does
// carry a goose Down that drops its tables, but the migrations db.FS actually
// embeds here are NOT yet squashed, so a round trip mirroring the final world
// cannot be driven against db.FS without reintroducing the obsolete
// multi-migration filter. What is retained: seed through the retained
// Reserve API and assert the root reads back byte-identical via GetBatchRoot
// on the fully migrated DB.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestBatchMigrationSeedAndReadBack: on the fully migrated (db.Connect)
// schema, a reserved batch root reads back byte-identically through
// GetBatchRoot.
func TestBatchMigrationSeedAndReadBack(t *testing.T) {
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))

	res, err := NewBatchStore(conn).Reserve(ctx, batchStoreTestReservation())
	require.NoError(t, err)

	after, err := db.New(conn).GetBatchRoot(ctx, db.GetBatchRootParams{RootID: res.Snapshot.Root.RootID, OwnerSessionID: "owner"})
	require.NoError(t, err)
	require.Equal(t, res.Snapshot.Root, after)
}
