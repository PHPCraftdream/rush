// Batch rehome (H4 lane P5, phase A): the ledger-free completion test helper
// moved out of work_ledger_test.go before its deletion. Surviving consumers:
// bgshell_observer_recheck_test.go (KEEP) and the batch parked-outcome
// fixtures.
package agent

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// batchFixtureConn opens a one-migration store connection on a fresh temp
// data dir (the deleted newTestAsyncJobStoreWithDataDir's batch replacement:
// the store file is rush_store_v1.db under dir, opened through db.Connect).
// Foreign keys stay ON; use batchFixtureConnNoFK for the thinnest fixtures
// that insert rows (e.g. history messages) without their parent rows.
func batchFixtureConn(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dir) })
	return conn, dir
}

// drainCompletions collects every completion sent on ch, non-blockingly.
func drainCompletions(ch chan AsyncCompletion) []AsyncCompletion {
	got := make([]AsyncCompletion, 0, 4)
	for {
		select {
		case c := <-ch:
			got = append(got, c)
		default:
			return got
		}
	}
}

// newBatchRuntimeCoordinator returns a bare test coordinator with the rehomed
// in-process coordination memory initialized (before the deletion the
// workLedger constructor set these; batch_rehome_coord_runtime.go owns them
// now).
func newBatchRuntimeCoordinator() *coordinator {
	c := &coordinator{}
	c.driverHints = newBatchDriverHints()
	c.supervision = newSupervisionRegistry()
	c.timeouts = newTimeoutService()
	c.subAgentDrivers = newSubAgentDriverRegistry()
	return c
}

// ensureFixtureSession inserts a bare sessions row for id IF it does not
// exist yet: the single-store schema's foreign keys (session_notices.owner,
// batch_nodes' owner_session_id) all anchor on sessions (id), so any fixture
// that seeds child rows under a literal id must have the row first. Callers
// may create the same id through the sessions service or another helper
// beforehand; the read-before-insert keeps the insert idempotent.
func ensureFixtureSession(t *testing.T, conn *sql.DB, id string) {
	t.Helper()
	q := db.New(conn)
	if _, err := q.GetSessionByID(t.Context(), id); err == nil {
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, err)
		return
	}
	createSessionRow(t, q, t.Context(), id)
}

// batchFixtureConnNoFK is batchFixtureConn with foreign keys disabled, the
// batch replacement of the deleted newTestAsyncJobStoreWithDataDir's
// FK-off connection for persistence-only fixtures.
func batchFixtureConnNoFK(t *testing.T) *sql.DB {
	t.Helper()
	conn, _ := batchFixtureConn(t)
	_, err := conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	return conn
}
