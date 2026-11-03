package cmd

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// An API service session (no messages, older than 7 days) owning only FINISHED
// batch rows: the rows cascade with their owner, so gc must keep it. Before the
// fix it collected the session and erased the history of finished roots.
func TestSessionsGc_KeepsAnOwnerOfFinishedBatchRows(t *testing.T) {
	a, shutdownSeed := isolatedGcEnv(t)
	f := seedGcBatchSession(t, a, "gc-batch-finished-owner")
	ctx := context.Background()
	_, err := a.DB().ExecContext(ctx, `UPDATE batch_nodes SET state='completed', outcome_state='completed', outcome_code='ok' WHERE owner_session_id=?`, f.owner)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx, `UPDATE sessions SET origin=? WHERE id=?`, string(message.OriginAPIService), f.owner)
	require.NoError(t, err)
	dataDir := a.Config().Options.DataDirectory
	shutdownSeed()

	_, stderr := runBatchGc(t, false)
	require.Contains(t, stderr, "kept session")

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var sessions, rows int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, f.owner).Scan(&sessions))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ?`, f.owner).Scan(&rows))
	require.Equal(t, 1, sessions, "the owner of finished batch rows is kept")
	require.Positive(t, rows, "and so are its batch rows")
}
