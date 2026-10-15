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

func TestD11ObserverRollbackPreservesEarlierCommittedEvidence(t *testing.T) {
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
	// A read-only transaction may roll back without any pre-update callback.
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(context.Background(), `SELECT count(*) FROM batch_nodes`).Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, tx.Rollback())
	d11FlushCommitted(t, db, state)
	evidence, err := state.validateCalls([]d11ToolIdentity{{call: "tool-call"}})
	require.NoError(t, err)
	require.Len(t, evidence, 3)
	require.NotEqual(t, evidence[0].Transaction, evidence[1].Transaction)
	require.NotEqual(t, evidence[1].Transaction, evidence[2].Transaction)
	require.NotEqual(t, evidence[0].Transaction, evidence[2].Transaction)
	state.mu.Lock()
	require.Equal(t, int64(3), state.commits)
	state.mu.Unlock()
}

func TestD11ObserverRollbackDiscardsCurrentWritesButKeepsEarlierCommit(t *testing.T) {
	state := &d11TransitionCounts{}
	db := newD11ObserverDB(t, state)
	state.registerCall("tool-call")
	committed, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = committed.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',1,'submit',0,1,1)`)
	require.NoError(t, err)
	_, err = committed.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root','root','tool-call')`)
	require.NoError(t, err)
	require.NoError(t, committed.Commit())

	rolledBack, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = rolledBack.ExecContext(context.Background(), `INSERT INTO batch_runner_checkpoints VALUES ('host','runner',2,'started',0,1,1) ON CONFLICT(host_id,runner_id) DO UPDATE SET sequence=excluded.sequence,event_type=excluded.event_type`)
	require.NoError(t, err)
	_, err = rolledBack.ExecContext(context.Background(), `INSERT INTO batch_nodes VALUES ('root','root','tool-call')`)
	require.NoError(t, err)
	require.NoError(t, rolledBack.Rollback())
	d11FlushCommitted(t, db, state)
	var event string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT event_type FROM batch_runner_checkpoints WHERE host_id='host' AND runner_id='runner'`).Scan(&event))
	require.Equal(t, "submit", event)

	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, int64(1), state.calls["tool-call"].transactions["submit"])
	require.Zero(t, state.calls["tool-call"].transactions["started"])
	require.Zero(t, state.calls["tool-call"].transactions["settled"])
	require.Equal(t, int64(1), state.commits)
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
	state.mu.Lock()
	require.Empty(t, state.calls["tool-call"].transactions)
	require.Empty(t, state.calls["tool-call"].root)
	require.Zero(t, state.commits)
	state.mu.Unlock()
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
	require.ErrorContains(t, err, "checkpoint transaction attribution ambiguous")
	state.mu.Lock()
	require.Contains(t, state.observerErrors, "event submit transaction touched 2 roots")
	state.mu.Unlock()
}
