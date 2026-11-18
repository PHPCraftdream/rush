package cmd

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

func newSessionFactsTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	return conn, db.New(conn)
}
