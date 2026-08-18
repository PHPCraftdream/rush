package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// TestSlotRenameMigration_PreservesExistingRows migrates a database only as
// far as the state that existed BEFORE the smart/fast rename, writes a
// session row with populated large_model_* / small_model_* values, then runs
// the remaining migrations and reads the row back under the new column names.
//
// A fresh-database test would prove nothing here: the point of using RENAME
// COLUMN rather than add-copy-drop is that live session state survives, and
// only a database that already HAS that state can demonstrate it.
func TestSlotRenameMigration_PreservesExistingRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "probe.db")
	conn, err := sql.Open("sqlite3", "file:"+dbPath)
	require.NoError(t, err)
	defer conn.Close()
	ctx := context.Background()

	goose.SetBaseFS(FS)
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.SetDialect("sqlite3"))

	// Stop one version short of the rename. Versions are the numeric
	// prefixes of the migration filenames; 20260816000002 is the last one
	// before 20260818000001_rename_model_slots_to_smart_fast.sql.
	require.NoError(t, goose.UpTo(conn, "migrations", 20260816000002))

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at,
		                      large_model_provider, large_model_id, large_model_reasoning_effort,
		                      small_model_provider, small_model_id, small_model_reasoning_effort)
		VALUES ('probe-session', 'probe', 0, 0, 0, 0, 1, 1,
		        'zai', 'glm-5.3', 'max',
		        'zai', 'glm-5-turbo', 'off')`)
	require.NoError(t, err, "seeding a row under the OLD column names must work at this migration state")

	require.NoError(t, goose.Up(conn, "migrations"), "the rename migration must apply to a database that already has rows")

	var smartProvider, smartID, smartEffort, fastProvider, fastID, fastEffort string
	err = conn.QueryRowContext(ctx, `
		SELECT smart_model_provider, smart_model_id, smart_model_reasoning_effort,
		       fast_model_provider, fast_model_id, fast_model_reasoning_effort
		FROM sessions WHERE id = 'probe-session'`).
		Scan(&smartProvider, &smartID, &smartEffort, &fastProvider, &fastID, &fastEffort)
	require.NoError(t, err, "the renamed columns must exist and be readable")

	require.Equal(t, "zai", smartProvider)
	require.Equal(t, "glm-5.3", smartID)
	require.Equal(t, "max", smartEffort)
	require.Equal(t, "zai", fastProvider)
	require.Equal(t, "glm-5-turbo", fastID)
	require.Equal(t, "off", fastEffort)

	// And the old names must be gone, not merely shadowed.
	var scrap string
	err = conn.QueryRowContext(ctx, `SELECT large_model_id FROM sessions WHERE id = 'probe-session'`).Scan(&scrap)
	require.Error(t, err, "large_model_id must no longer exist after the rename")
}
