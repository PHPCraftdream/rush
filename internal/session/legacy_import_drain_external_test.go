package session_test

// End-to-end proof (external test package: production API only, no db-package
// test helpers) that a legacy orphan_call_outbox row stuck in the retired
// pre-P0-4 'processing' claim state survives the legacy import with its
// payload verbatim, is normalized back to 'pending' by the import
// (legacy_import.go:638-641), and is then genuinely drained by the REAL
// session service (DrainOrphanOutboxEntry) into session_run_queue.
//
// Chain under test: legacy 'processing' row -> ImportLegacyStore (processing
// -> pending, call_data preserved) -> session.NewService over the imported
// store_v1 DB -> DrainOrphanOutboxEntry moves the row into session_run_queue
// with call_data unchanged and deletes the outbox row -> idempotent second
// drain reports false.

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/db/storepath"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	conn.SetMaxOpenConns(1) // SQLite: one writer
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestLegacyImportNormalizedProcessingOutboxIsDrainedEndToEnd(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	// 1. Build a legacy store at the legacy path using the frozen importer-owned chain.
	legacyConn := openSQLite(t, storepath.DataDirPath(dataDir, storepath.LegacyFilename))
	legacyMigrations, err := fs.Sub(os.DirFS(filepath.Join("..", "db")), "legacymigrations")
	require.NoError(t, err)
	legacyProvider, err := goose.NewProvider(goose.DialectSQLite3, legacyConn, legacyMigrations, goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)
	_, err = legacyProvider.Up(ctx)
	require.NoError(t, err)
	_, err = legacyConn.ExecContext(ctx,
		`INSERT INTO sessions (id,title,updated_at,created_at,cancel_requested) VALUES ('s','s',1700000000,1700000000,0)`)
	require.NoError(t, err)
	// The stranded pre-P0-4 claim: 'processing' is NOT scannable by the
	// drain (it only lists 'pending'), so without the import normalization
	// this call would be stranded forever.
	_, err = legacyConn.ExecContext(ctx,
		`INSERT INTO orphan_call_outbox (id,session_id,call_data,status,created_at,updated_at) VALUES ('o-claim','s','{"x":1}','processing',1,2)`)
	require.NoError(t, err)
	require.NoError(t, legacyConn.Close())

	// 2. Import into store_v1. The destination schema is caller-supplied
	// (LegacyImportOptions.Schema, legacy_import.go:32-34); the package-db
	// fixture source is os.DirFS("testdata/store_v1")
	// (store_v1_queries_isolation_test.go:137-139) -- the same read-only
	// directory, addressed relative to THIS package's test CWD.
	result, err := db.ImportLegacyStore(ctx, dataDir, db.LegacyImportOptions{
		Schema: os.DirFS(filepath.Join("..", "db", "testdata", "store_v1")),
	})
	require.NoError(t, err)
	require.Equal(t, db.LegacyImportImported, result.Status)

	// 3. The import normalized 'processing' back to 'pending', payload
	// verbatim (the exact rows the package-db test pins at
	// legacy_import_test.go:415-420).
	importedConn := openSQLite(t, storepath.StoreV1Path(dataDir))
	var status, outboxCallData string
	require.NoError(t, importedConn.QueryRowContext(ctx,
		`SELECT status, call_data FROM orphan_call_outbox WHERE id='o-claim'`).Scan(&status, &outboxCallData))
	require.Equal(t, "pending", status, "the import must reset the retired 'processing' claim so the drain can scan it")
	require.Equal(t, `{"x":1}`, outboxCallData, "the payload must survive the import verbatim")

	// 4. The REAL session service over the imported store drains the row:
	// orphan_call_outbox -> session_run_queue, call_data unchanged, outbox
	// row gone.
	svc := session.NewService(db.New(importedConn), importedConn)
	drained, err := svc.DrainOrphanOutboxEntry(ctx, "o-claim")
	require.NoError(t, err)
	require.True(t, drained, "the normalized pending entry must drain")

	var queueCallData string
	require.NoError(t, importedConn.QueryRowContext(ctx,
		`SELECT call_data FROM session_run_queue WHERE id='o-claim'`).Scan(&queueCallData))
	require.Equal(t, `{"x":1}`, queueCallData, "the drained entry must land in the run queue with its payload unchanged")

	var outboxLeft int
	require.NoError(t, importedConn.QueryRowContext(ctx,
		`SELECT count(*) FROM orphan_call_outbox WHERE id='o-claim'`).Scan(&outboxLeft))
	require.Zero(t, outboxLeft, "the drained outbox row must be gone")

	// 5. Idempotence: draining an already-drained id is a no-op.
	drainedAgain, err := svc.DrainOrphanOutboxEntry(ctx, "o-claim")
	require.NoError(t, err)
	require.False(t, drainedAgain, "a second drain of the same id must report nothing to do")
}
