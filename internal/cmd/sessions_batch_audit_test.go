package cmd

// R12-B part 3 coverage: with RUSH_BATCH_RUNTIME on, `rush sessions audit`
// counts batch facts through the R10a live reader and the R10c
// BatchHistoryStateCounts query over its own read-only handle, and a missing
// legacy async_jobs table is a REPORTED normal state — never a silently
// disabled rule. Flag off, today's behavior is byte-for-byte unchanged,
// including the missing-table disable path. Fixtures are real migrated
// databases (db.Connect on a temp dir, never :memory:) seeded through
// session.BatchStore.Reserve with a kind=batch root and task children.

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

// dropLegacyAsyncJobs drops the migrated async_jobs table through a fresh
// writable connection (TEST DB ONLY): the exact state a pre-batch database
// is in, reached the literal way an operator would.
func dropLegacyAsyncJobs(t *testing.T, dataDir string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "DROP TABLE async_jobs")
	require.NoError(t, err)
	require.NoError(t, db.ReleaseConn(conn))
}

// TestSessionsAudit_BatchFlagOn_MissingLegacyTableReported is the flagship
// case: flag on, async_jobs dropped after migration. The audit must still
// run, must NOT silently disable unfinished_jobs, must report the legacy
// table's absence with the stable wording, and must count the seeded batch
// nodes (state distribution, held root, task counts).
//
// Static-trace: db.Connect ran the real migrations (batch_nodes present, so
// the flag-on schema guard passes); the audit opens mode=ro lazily, so the
// DROP before the run is visible to its PRAGMA table_info probe; the seed
// satisfies the batch_nodes FKs (root kind='batch' with tool_call_id and
// input_hash) and registered its own host via EnsureHost.
func TestSessionsAudit_BatchFlagOn_MissingLegacyTableReported(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, sessionID, f := seedBatchAuditDB(t, "audit-batch-nolegacy")
	dropLegacyAsyncJobs(t, dataDir)

	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	t.Logf("flag-on audit with dropped async_jobs:\n%s", stdout)

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
	require.NotNil(t, reports[0].Batch, "the flag-on JSON must carry the batch block")
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

// TestSessionsAudit_BatchFlagOn_CountsAndTerminalRoot: flag on with the
// legacy table present, the rule counts from batch_nodes (the rule status
// says so), and a root projected to a terminal state produces no
// unfinished_jobs finding while the state counts still show it.
func TestSessionsAudit_BatchFlagOn_CountsAndTerminalRoot(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	dataDir, sessionID, f := seedBatchAuditDB(t, "audit-batch-terminal")
	projectBatchAuditRoot(t, f, "cancelling", 2)

	// Held (cancelling) root: one finding, named with the tool call and state.
	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	require.Contains(t, stdout, auditBatchRuleBatchStatus)
	require.Contains(t, stdout,
		auditFindingLine("unfinished_jobs", sessionID, f.toolCallID+":cancelling", 1))
	require.NotNil(t, reports[0].Batch)
	require.False(t, reports[0].Batch.LegacyAsyncJobsAbsent,
		"the legacy table is present; the absence flag must stay off")

	// Terminal root: nothing unfinished, but the counts still show the work.
	projectBatchAuditRoot(t, f, "completed", 3)
	stdout, reports = auditRunRulesDB(t, dataDir, "--all")
	require.NotContains(t, stdout, "unfinished_jobs",
		"a completed root is not unfinished work")
	require.NotNil(t, reports[0].Batch)
	facts := reports[0].Batch.Sessions[sessionID]
	require.False(t, facts.Live)
	require.Equal(t, 0, facts.HeldRoots)
	require.Equal(t, 2, facts.Tasks.Done)
	require.Equal(t, int64(2), facts.StateCounts["completed"])
}

// TestSessionsAudit_BatchFlagOff_MissingLegacyTableUnchanged covers (3): the
// same dropped-table database with the flag unset must behave exactly as
// today — the unfinished_jobs rule silently absent, no batch block in the
// JSON, no batch lines in the text output.
//
// Static-trace: the flag-off path never calls auditScanBatchFacts, so
// report.Batch stays nil and printText adds nothing.
func TestSessionsAudit_BatchFlagOff_MissingLegacyTableUnchanged(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	dataDir, _, f := seedBatchAuditDB(t, "audit-batch-flagoff-drop")
	dropLegacyAsyncJobs(t, dataDir)

	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	t.Logf("flag-off audit with dropped async_jobs:\n%s", stdout)

	require.NotContains(t, stdout, "unfinished_jobs", "flag off: no table, no rule (today's behavior)")
	require.NotContains(t, stdout, auditLegacyAbsentRuleStatus)
	require.NotContains(t, stdout, "batch runtime on")
	require.Nil(t, reports[0].Batch, "flag-off JSON must stay byte-identical: no batch block")
	require.Contains(t, stdout, "sessions (1)", "the session itself is still reported")
	_ = f
}

// TestSessionsAudit_BatchFlagOff_NormalDBUnchanged covers (4): on an
// untouched migrated database with the flag off, the report carries no batch
// block and the seeded async-free audit output is what today produces.
func TestSessionsAudit_BatchFlagOff_NormalDBUnchanged(t *testing.T) {
	require.False(t, batchRuntimeFlagOnForTest(), "precondition: flag must be unset")
	dataDir, sessionID, _ := seedBatchAuditDB(t, "audit-batch-flagoff-normal")

	stdout, reports := auditRunRulesDB(t, dataDir, "--all")
	require.Contains(t, stdout, sessionID)
	require.Nil(t, reports[0].Batch, "flag off: no batch block on a normal database either")
	require.Empty(t, reports[0].Findings, "no async rows and no flag: no findings")
}
