package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func newD11ObserverDB(t *testing.T, state *d11TransitionCounts) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "observer.db")+"?_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE batch_nodes (node_id TEXT, root_id TEXT, tool_call_id TEXT)`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `CREATE TABLE batch_runner_checkpoints (host_id TEXT, runner_id TEXT, sequence INTEGER, event_type TEXT, final INTEGER, created_at INTEGER, updated_at INTEGER, PRIMARY KEY(host_id,runner_id))`)
	require.NoError(t, err)
	d11InstallHooks(t, db, state)
	return db
}

func TestD11ObserverCommitHookCorrelatesCheckpointAfterNodeProjection(t *testing.T) {
	state := &d11TransitionCounts{}
	db := newD11ObserverDB(t, state)
	state.registerCall("tool-call")
	for seq, event := range []string{"submit", "started", "settled"} {
		tx, err := db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',?,?,0,1,1) ON CONFLICT(host_id,runner_id) DO UPDATE SET sequence=excluded.sequence,event_type=excluded.event_type`, seq+1, event)
		require.NoError(t, err)
		_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root','root','tool-call')`)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	d11FlushCommitted(t, db, state)
	evidence, err := state.validateCalls([]d11ToolIdentity{{call: "tool-call"}})
	require.NoError(t, err)
	require.Len(t, evidence, 3)
	require.NotEqual(t, evidence[0].Transaction, evidence[1].Transaction)
	require.NotEqual(t, evidence[1].Transaction, evidence[2].Transaction)
	require.NotEqual(t, evidence[0].Transaction, evidence[2].Transaction)
}

func TestD11ObserverRollbackDiscardsCheckpointEvidence(t *testing.T) {
	state := &d11TransitionCounts{}
	db := newD11ObserverDB(t, state)
	state.registerCall("tool-call")
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',1,'submit',0,1,1)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root','root','tool-call')`)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	d11FlushCommitted(t, db, state)
	_, err = state.validateCalls([]d11ToolIdentity{{call: "tool-call"}})
	require.Error(t, err)
}

func TestD11ObserverFlagsMultipleEventsInOneTransaction(t *testing.T) {
	state := &d11TransitionCounts{}
	db := newD11ObserverDB(t, state)
	state.registerCall("tool-call")
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	for seq, event := range []string{"submit", "started", "settled"} {
		_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',?,?,0,1,1) ON CONFLICT(host_id,runner_id) DO UPDATE SET sequence=excluded.sequence,event_type=excluded.event_type`, seq+1, event)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root','root','tool-call')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	d11FlushCommitted(t, db, state)
	_, err = state.validateCalls([]d11ToolIdentity{{call: "tool-call"}})
	require.Error(t, err)
}

func TestD11ObserverRejectsCheckpointTouchingMultipleRoots(t *testing.T) {
	state := &d11TransitionCounts{}
	db := newD11ObserverDB(t, state)
	state.registerCall("call-a")
	state.registerCall("call-b")
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',1,'submit',0,1,1)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root-a','root-a','call-a'), ('root-b','root-b','call-b')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	d11FlushCommitted(t, db, state)
	_, err = state.validateCalls([]d11ToolIdentity{{call: "call-a"}})
	require.Error(t, err)
}
