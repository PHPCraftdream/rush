package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionRoleContractMigrationPreservesLegacyRoleRows(t *testing.T) {
	conn, err := openDB(filepath.Join(t.TempDir(), "role-contract-migration.db"))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(context.Background(), `
		CREATE TABLE sessions (id TEXT PRIMARY KEY);
		CREATE TABLE session_agent_model_role (
			session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			requested_role TEXT NOT NULL CHECK (requested_role IN ('', 'smart', 'fast', 'worker', 'reviewer'))
		);
		INSERT INTO sessions(id) VALUES ('legacy-session');
		INSERT INTO session_agent_model_role(session_id, requested_role) VALUES ('legacy-session', 'worker');
	`)
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("migrations", "20261110000001_session_role_contracts.sql"))
	require.NoError(t, err)
	up := strings.SplitN(string(migration), "-- +goose Down", 2)[0]
	_, err = conn.ExecContext(context.Background(), up)
	require.NoError(t, err)
	var role string
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT requested_role FROM session_agent_model_role WHERE session_id='legacy-session'`).Scan(&role))
	require.Equal(t, "worker", role)
	require.ElementsMatch(t, []string{"session_id", "contract_json"}, tableColumns(t, conn, "session_role_contracts"))
	require.ElementsMatch(t, []string{"session_id", "logical_call_id", "contract_json"}, tableColumns(t, conn, "session_root_role_contracts"))
	var count int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM session_root_role_contracts`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT count(*) FROM session_role_contracts`).Scan(&count))
	require.Zero(t, count)
}
