// F6a-3 stage test-only dual-mode readback helpers. The f6 helpers seed
// fixtures; these READ the selected authority's job facts for work the
// production loop itself created (bash jobs, drains), which flag ON writes
// to batch_nodes and never to async_jobs. All queries are read-only.
package app

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// f6a3ReactionDebtExists reads the unreacted completion-debt predicate
// through the selected authority: BatchReactionDebtExists over batch_nodes
// flag ON, the legacy AsyncJobStore predicate flag OFF.
func f6a3ReactionDebtExists(t *testing.T, application *App, owner string) bool {
	t.Helper()
	if f6BatchOn() {
		debt, err := session.NewBatchStore(application.DB()).BatchReactionDebtExists(context.Background(), owner)
		require.NoError(t, err)
		return debt
	}
	debt, err := application.asyncJobStore.ReactionDebtExists(context.Background(), owner)
	require.NoError(t, err)
	return debt
}

// f6a3ReactionDebtExistsOK is the non-failing variant for require.Eventually
// conditions: it returns (debt, ok) and reports ok=false on any read error
// instead of aborting the test on a transient one.
func f6a3ReactionDebtExistsOK(t *testing.T, application *App, owner string) (debt, ok bool) {
	t.Helper()
	if f6BatchOn() {
		debt, err := session.NewBatchStore(application.DB()).BatchReactionDebtExists(context.Background(), owner)
		return debt, err == nil
	}
	debt, err := application.asyncJobStore.ReactionDebtExists(context.Background(), owner)
	return debt, err == nil
}

// f6a3NoWorkForCall asserts the tool call NEVER became work in the selected
// authority: no legacy row flag OFF (a refused wait never claims), NO batch
// row in ANY state -- terminal included -- flag ON (a refused wait never
// reserves; a call that executed, inline or detached, always leaves a root
// row behind).
func f6a3NoWorkForCall(t *testing.T, application *App, owner, callID string) {
	t.Helper()
	if !f6BatchOn() {
		_, err := application.asyncJobStore.Get(context.Background(), owner, callID)
		require.Error(t, err, "a refused wait starts no job: no async_jobs row for %s", callID)
		return
	}
	var rows int
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?`,
		owner, callID).Scan(&rows))
	require.Equal(t, 0, rows, "a refused wait must start no batch work at all for %s", callID)
}

// f6a3JobRunning reports whether the tool call's job is claimed and running
// through the selected authority: the legacy row's state flag OFF, the live
// batch root row's state flag ON (the root row is the one carrying the tool
// call id; child rows never do, migrations/20261012000001 CHECK).
func f6a3JobRunning(t *testing.T, application *App, owner, callID string) bool {
	t.Helper()
	if !f6BatchOn() {
		row, err := application.asyncJobStore.Get(context.Background(), owner, callID)
		if err != nil {
			return false
		}
		return row.State == "running"
	}
	var state string
	err := application.DB().QueryRowContext(context.Background(),
		`SELECT state FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ? AND node_id = root_id`,
		owner, callID).Scan(&state)
	if err != nil {
		return false
	}
	return state == "running"
}

// f6a3JobWakeAttempts reads the job's wake-attempt count through the
// selected authority: the legacy row flag OFF, the maximum over the root's
// batch nodes flag ON (wake attempts are accounted on the selected child).
func f6a3JobWakeAttempts(t *testing.T, application *App, owner, callID string) int64 {
	t.Helper()
	if f6BatchOn() {
		var attempts int64
		require.NoError(t, application.DB().QueryRowContext(context.Background(),
			`SELECT COALESCE(MAX(wake_attempts), 0) FROM batch_nodes WHERE root_id IN`+
				` (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?)`,
			owner, callID).Scan(&attempts))
		return attempts
	}
	row, err := application.asyncJobStore.Get(context.Background(), owner, callID)
	require.NoError(t, err)
	return row.WakeAttempts
}
