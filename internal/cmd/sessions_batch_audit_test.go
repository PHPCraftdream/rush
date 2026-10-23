package cmd

// R12-B part 3 coverage: `rush sessions audit` counts batch facts through
// the R10a live reader and the R10c BatchHistoryStateCounts query over its
// own read-only handle, and the ABSENT legacy async_jobs table (the only
// state a store-v1 database can be in) is a REPORTED normal state — never a
// silently disabled rule. Fixtures are real migrated databases (db.Connect
// on a temp dir, never :memory:) seeded through session.BatchStore.Reserve
// with a kind=batch root and task children.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// seedBatchAuditDB stands up an isolated jobs env with one session carrying
// a seeded batch root (kind=batch, tool call + input hash, two task
// children) on the app's own registered host, then shuts the app down so the
// audit's read-only open is the only handle during the run. The caller owns
// the returned fixture identities.
func seedBatchAuditDB(t *testing.T, id string) (dataDir, sessionID string, f batchReadFixture) {
	t.Helper()
	a, dir := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, id, "batch audit session")
	require.NoError(t, err)
	f = seedBatchReadRoot(t, a, sess.ID)
	require.NotEmpty(t, f.host, "static-trace: the seed registered its own host via EnsureHost")
	a.Shutdown()
	return dir, sess.ID, f
}

// TestSessionsAudit_MissingLegacyTableReported is the flagship
// case: a store-v1 database has no async_jobs table at all. The audit must still
// run, must NOT silently disable unfinished_jobs, must report the legacy
// table's absence with the stable wording, and must count the seeded batch
// nodes (state distribution, held root, task counts).
//
// Static-trace: db.Connect ran the real migrations (batch_nodes present, so
// the schema guard passes); the audit's PRAGMA table_info probe sees no
// async_jobs table, so the absence flag is on; the seed satisfies the
// batch_nodes FKs (root kind='batch' with tool_call_id and
// input_hash) and registered its own host via EnsureHost.
func TestSessionsAudit_MissingLegacyTableReported(t *testing.T) {
	dataDir, sessionID, f := seedBatchAuditDB(t, "audit-batch-nolegacy")

	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	t.Logf("audit of a legacy-free store:\n%s", stdout)

	// The rule is reported, not silently disabled and not an error.
	require.Contains(t, stdout, auditLegacyAbsentRuleStatus,
		"the missing legacy table must be reported with the stable wording")
	require.NotContains(t, stdout, "unrecognized schema",
		"a missing optional legacy table is not a schema issue")

	// The batch facts are counted: the seeded root is pending/held with two
	// pending task children, so the rule fires on batch evidence.
	require.Contains(t, stdout,
		auditFindingLine("unfinished_jobs", sessionID, f.toolCallID+":pending", 1),
		"the held root must feed the unfinished_jobs rule")

	require.Len(t, reports, 1)
	require.NotNil(t, reports[0].Batch, "the JSON must carry the batch block")
	batch := reports[0].Batch
	require.True(t, batch.LegacyAsyncJobsAbsent)
	require.Equal(t, auditLegacyAbsentRuleStatus, batch.UnfinishedJobsRule)
	facts := batch.Sessions[sessionID]
	require.True(t, facts.Live, "a held root must read as live")
	require.False(t, facts.Unknown, "a complete read must not claim unknown")
	require.Equal(t, 1, facts.Roots)
	require.Equal(t, 1, facts.HeldRoots)
	require.Equal(t, 2, facts.Tasks.Pending, "two seeded task children")
	require.Equal(t, int64(2), facts.StateCounts["pending"],
		"BatchHistoryStateCounts must count the task nodes per state")
	require.False(t, facts.GuardDebt, "a fresh fixture has no completion debt")
	require.Empty(t, reports[0].SchemaIssues)
}

// projectBatchAuditRoot is projectBatchRoot with an explicit sequence, so one
// test can project the same root twice (each projection must move forward).
func projectBatchAuditRoot(t *testing.T, f batchReadFixture, state string, sequence int64) {
	t.Helper()
	ctx := context.Background()
	var updates []db.ProjectBatchNodeParams
	for _, id := range []string{f.rootID, f.nodeA, f.nodeB} {
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: id, State: state, ControlState: "none"})
	}
	require.NoError(t, f.store.Project(ctx, session.BatchProjection{
		Host: f.host, RunnerID: "batch-read-runner", Sequence: sequence,
		Roots: []session.BatchRootProjection{{Owner: f.owner, Root: f.rootID, Updates: updates}},
	}))
}

// TestSessionsAudit_CountsAndTerminalRoot: the rule counts from batch_nodes
// (the rule status says so), and a root projected to a terminal state
// produces no unfinished_jobs finding while the state counts still show it.
func TestSessionsAudit_CountsAndTerminalRoot(t *testing.T) {
	dataDir, sessionID, seed := seedBatchAuditDB(t, "audit-batch-terminal")
	// The seed app is shut down inside seedBatchAuditDB (its pool teardown
	// closes handles at refcount zero), so projection needs a FRESH store on
	// the same data dir via the Connect path: Connect re-runs only pending
	// migrations (no-op here) and bumps the pool refcount, keeping the
	// handle open through both projections.
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	f := seed
	f.store = session.NewBatchStore(conn)
	t.Cleanup(func() { _ = db.ReleaseConn(conn) })
	projectBatchAuditRoot(t, f, "cancelling", 2)

	// Held (cancelling) root: one finding, named with the tool call and state.
	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	require.Contains(t, stdout, auditLegacyAbsentRuleStatus)
	require.Contains(t, stdout,
		auditFindingLine("unfinished_jobs", sessionID, f.toolCallID+":cancelling", 1))
	require.NotNil(t, reports[0].Batch)

	// Terminal root: no unfinished_jobs FINDING (the rule status line still
	// says where it counts from), but the counts still show the work.
	projectBatchAuditRoot(t, f, "completed", 3)
	stdout, reports = auditRunRulesDB(t, dataDir, "--all")
	// Finding lines are "unfinished_jobs: session ..."; the rule STATUS line
	// is "unfinished_jobs counted..." with no colon-space. Only findings are
	// forbidden, never the status line.
	require.NotContains(t, stdout, "unfinished_jobs: ",
		"a completed root produces no unfinished_jobs finding")
	require.Contains(t, stdout, auditLegacyAbsentRuleStatus,
		"the batch status line (counted from batch_nodes) stays")
	require.NotNil(t, reports[0].Batch)
	facts := reports[0].Batch.Sessions[sessionID]
	require.False(t, facts.Live)
	require.Equal(t, 0, facts.HeldRoots)
	// Terminal roots are excluded from the live read (batch_read.sql filters
	// to nonterminal roots), so Tasks totals come only from live roots: the
	// completed tasks are asserted via the per-state counts instead.
	require.Equal(t, int64(2), facts.StateCounts["completed"])
}
