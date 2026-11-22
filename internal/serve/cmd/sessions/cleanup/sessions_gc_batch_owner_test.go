package sessclean

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

func TestSessionsGc_KeepsAnOwnerOfFinishedBatchRows(t *testing.T) {
	a, shutdownSeed, cmd := isolatedGcBatchOwnerEnv(t)
	owner := seedGcFinishedBatchOwner(t, a)
	dataDir := a.Config().Options.DataDirectory
	shutdownSeed()

	_, stderr := runGcBatchOwnerCommand(t, cmd)
	require.Contains(t, stderr, "kept session")

	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	defer func() { _ = db.ReleaseConn(conn) }()
	var sessions, rows int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, owner).Scan(&sessions))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ?`, owner).Scan(&rows))
	require.Equal(t, 1, sessions, "the owner of finished batch rows is kept")
	require.Positive(t, rows, "and so are its batch rows")
}
