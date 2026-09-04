package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrate_AppliesEmbeddedMigrations(t *testing.T) {
	conn, err := openDB(t.TempDir() + "/migrate.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, Migrate(context.Background(), conn))

	var version int64
	err = conn.QueryRowContext(context.Background(),
		"SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY version_id DESC LIMIT 1",
	).Scan(&version)
	require.NoError(t, err)
	require.Positive(t, version)
}
