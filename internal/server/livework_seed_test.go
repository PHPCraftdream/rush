package server

// F9: dual-mode live-work test seeding. The package's live-work tests used
// to seed the legacy async_jobs rows directly; under RUSH_BATCH_RUNTIME the
// production readers ignore that table and read batch_nodes, so the same
// facts must be seeded there. seedLiveWorkRow seeds the flag-appropriate
// store: OFF uses the real AsyncJobStore exactly as before (byte-identical
// legacy rows); ON raw-INSERTs one batch root (plus its task child) with the
// column set insertBatchRootAndTask established for the package.
//
// projectLiveWorkRoot is the batch-side equivalent of a store.Transition to
// a terminal state: it settles every row of one root (the tool-call root row
// and its task child) to the given state.

import (
	"context"
	"database/sql"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// liveWorkSeedSpec is one live-work row to seed, in either mode.
type liveWorkSeedSpec struct {
	owner          string
	toolCallID     string
	taskKind       string
	childSessionID string
	state          string
	cause          string
	updatedAt      int64
}

// liveWorkNodeID is the batch root node id for one spec. It carries the
// owner because node_id is a global PRIMARY KEY while (owner, tool_call_id)
// is only unique together — two sessions may reuse one tool call id.
func liveWorkNodeID(spec liveWorkSeedSpec) string {
	return "lw-" + spec.toolCallID + "-" + spec.owner
}

// seedLiveWorkRow seeds one live-work row through the flag-appropriate
// store. OFF: a real AsyncJobStore claim (and transition when the row is
// not running). ON: one batch root row, plus one task child when the spec
// carries a task kind, satisfying batch_nodes' CHECK/FK invariants with the
// same column set insertBatchRootAndTask uses.
func seedLiveWorkRow(t *testing.T, a *appPkg.App, spec liveWorkSeedSpec) {
	t.Helper()
	ctx := t.Context()
	if batchRuntimeEnabled() {
		require.NoError(t, insertLiveWorkBatchRows(ctx, a.DB(), spec))
		return
	}
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	kind := session.JobKindCommand
	if spec.taskKind == "agent" || spec.taskKind == "agentic_fetch" {
		kind = session.JobKindAgent
	}
	_, err := store.Claim(ctx, session.ClaimParams{
		Owner: spec.owner, ToolCallID: spec.toolCallID, Kind: kind,
		Input: spec.toolCallID, ToolName: spec.taskKind, ChildSessionID: spec.childSessionID,
	})
	require.NoError(t, err)
	if spec.state != "running" {
		_, err = store.Transition(ctx, session.TransitionParams{
			Owner: spec.owner, ToolCallID: spec.toolCallID, State: spec.state,
			NoticeKind: spec.cause, ResultSummary: "",
		})
		require.NoError(t, err)
	}
	if spec.updatedAt != 0 {
		_, err = a.DB().ExecContext(ctx, "UPDATE async_jobs SET updated_at = ? WHERE tool_call_id = ?", spec.updatedAt, spec.toolCallID)
		require.NoError(t, err)
	}
}

// insertLiveWorkBatchRows writes one spec as batch_nodes rows over any
// writable *sql.DB (the app's own handle or a foreign one).
func insertLiveWorkBatchRows(ctx context.Context, dbh *sql.DB, spec liveWorkSeedSpec) error {
	rootID := liveWorkNodeID(spec)
	updated := spec.updatedAt
	if updated == 0 {
		updated = 1700000001
	}
	_, err := dbh.ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash, mode, state, cause, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', 0, 'batch', ?, ?, 'parallel', ?, ?, 1700000000, ?)`,
		rootID, rootID, spec.owner, spec.toolCallID, "hash-"+spec.toolCallID, spec.state, spec.cause, updated)
	if err != nil {
		return err
	}
	if spec.taskKind == "" {
		return nil
	}
	_, err = dbh.ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind, task_kind, task_payload, child_session_id, state, cause, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', ?, 0, 'task', ?, '{}', ?, ?, ?, 1700000000, ?)`,
		rootID+"-1", rootID, spec.owner, rootID, spec.taskKind,
		sql.NullString{String: spec.childSessionID, Valid: spec.childSessionID != ""},
		spec.state, spec.cause, updated)
	return err
}

// projectLiveWorkRoot settles one root — its root row and every row of that
// root — to the given (terminal) state: the batch-side equivalent of a
// store.Transition used by the legacy tests' terminal phases.
func projectLiveWorkRoot(t *testing.T, a *appPkg.App, owner, toolCallID, state string) {
	t.Helper()
	_, err := a.DB().ExecContext(t.Context(), `UPDATE batch_nodes SET state = ?1, updated_at = 1700000002
		WHERE owner_session_id = ?2 AND (tool_call_id = ?3 OR root_id IN
			(SELECT root_id FROM batch_nodes WHERE owner_session_id = ?2 AND tool_call_id = ?3))`,
		state, owner, toolCallID)
	require.NoError(t, err)
}
