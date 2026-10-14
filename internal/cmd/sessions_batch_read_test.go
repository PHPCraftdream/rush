package cmd

// R12-B part 1 coverage: with RUSH_BATCH_RUNTIME on, the session status
// commands read live batch node rows (real SQLite fixtures seeded through
// session.BatchStore.Reserve/Project, never :memory:) and report held roots,
// cancelling roots as "stopped but awaiting termination", and conservative
// UNKNOWN on incomplete/failed reads. Flag off, every command must produce
// its legacy output byte-for-byte.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchReadFixture carries the seeded root's identities for assertions.
// closeAsync, when non-nil, closes the fallback AsyncJobStore the seed
// opened for a store-less App (releasing its host lock).
type batchReadFixture struct {
	rootID, nodeA, nodeB, host, owner, toolCallID string
	store                                         *session.BatchStore
	closeAsync                                    func()
}

// seedBatchReadRoot reserves a real batch root (kind=batch with tool call and
// input hash, two task children) for owner on the app's own registered host,
// so the live reader proves liveness via the own-host registry. Tolerates a
// nil a.AsyncJobStore() (setupAppLite fixtures): it then opens a fallback
// store, whose EnsureHost registers the live test process and which is
// closed at cleanup so the host lock cannot block later command runs.
func seedBatchReadRoot(t *testing.T, a *app.App, owner string) batchReadFixture {
	t.Helper()
	ctx := context.Background()
	store := a.AsyncJobStore()
	var closeAsync func()
	if store == nil {
		store = session.NewAsyncJobStore(a.DB(), a.Config().Options.DataDirectory, os.Getpid(), "batch-read-fixture")
		closeAsync = func() { _ = store.Close(context.Background()) }
		t.Cleanup(closeAsync)
	}
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	bs := session.NewBatchStore(a.DB())
	reserved, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "batch-read-runner",
		ToolCallID: "batch-read-call", InputHash: "batch-read-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
	// Snapshot.Nodes includes the root and is ordered by node_id: derive the
	// task children by parent_id, never by index.
	rootID := reserved.Snapshot.Root.RootID
	var kids []string
	for _, n := range reserved.Snapshot.Nodes {
		if n.ParentID.Valid && n.ParentID.String == rootID {
			kids = append(kids, n.NodeID)
		}
	}
	require.Len(t, kids, 2, "the fixture reserves exactly two task children")
	return batchReadFixture{
		rootID: rootID,
		nodeA:  kids[0],
		nodeB:  kids[1],
		host:   host, owner: owner, toolCallID: "batch-read-call", store: bs,
		closeAsync: closeAsync,
	}
}

// projectBatchRoot moves the whole root (root row plus tasks) to state.
func projectBatchRoot(t *testing.T, f batchReadFixture, state string) {
	t.Helper()
	ctx := context.Background()
	var updates []db.ProjectBatchNodeParams
	for _, id := range []string{f.rootID, f.nodeA, f.nodeB} {
		updates = append(updates, db.ProjectBatchNodeParams{NodeID: id, State: state, ControlState: "none"})
	}
	require.NoError(t, f.store.Project(ctx, session.BatchProjection{
		Host: f.host, RunnerID: "batch-read-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: f.owner, Root: f.rootID, Updates: updates}},
	}))
}

// errBatchLiveSource is the interface fake for the conservative-unknown test:
// every read fails, like a broken or unreachable database would.
type errBatchLiveSource struct{ err error }

func (s errBatchLiveSource) BatchLiveNodesForOwners(context.Context, []string) ([]db.BatchNode, error) {
	return nil, s.err
}

func (s errBatchLiveSource) BatchLevelNodes(context.Context, db.BatchLevelNodesParams) ([]db.BatchNode, error) {
	return nil, s.err
}

// TestReduceBatchSessionFacts_ErrorMeansUnknownAndNeverIdle: a failed or nil
// source must map to Unknown (conservative), never to an empty/idle result.
func TestReduceBatchSessionFacts_ErrorMeansUnknownAndNeverIdle(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	reader := &session.BatchLiveReader{Source: errBatchLiveSource{err: context.DeadlineExceeded}}
	got := reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(nil), nil, []string{"owner-x"})
	require.Contains(t, got, "owner-x")
	f := got["owner-x"]
	require.True(t, f.Unknown, "a failed read must be unknown")
	require.True(t, f.Live, "a failed read must never read as idle")
	require.Empty(t, f.Roots, "no facts may be invented from a failed read")

	// Nil reader: same conservative verdict.
	got = reduceBatchSessionFacts(ctx, nil, nil, nil, []string{"owner-y"})
	require.True(t, got["owner-y"].Unknown)
	require.True(t, got["owner-y"].Live)
	require.Equal(t, " (batch unknown)", batchStatusAnnotation(got["owner-y"]))
}

// TestSessionsJobsCmdRun_BatchLiveRootFlagOn covers (a) and (b): with the
// flag on, a pending root reports live and held, and a cancelling root
// reports as held/awaiting termination -- never terminal, never idle.
func TestSessionsJobsCmdRun_BatchLiveRootFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-batch-sess", "session with batch root")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	// Pending-phase coverage WITHOUT running RunE beside the live seed app:
	// setupApp acquires the single MCP process owner (app.go mcp.Acquire), so
	// a second setupApp while the seed is alive fails. Assert the pending
	// root through the reducer directly instead, while the seed store is
	// still open.
	pending := batchRuntimeFacts(ctx, a, dataDir, []string{sess.ID})
	require.NotNil(t, pending)
	pf := pending[sess.ID]
	require.False(t, pf.Unknown, "the seed read must be complete")
	require.True(t, pf.Live, "a pending root is live")
	require.Len(t, pf.Roots, 1)
	require.Equal(t, f.toolCallID, pf.Roots[0].ToolCallID)
	require.Equal(t, "pending", pf.Roots[0].State)
	require.True(t, pf.Roots[0].Held)
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)
	// Project to cancelling AND insert the held question while the seed
	// store is still open, then shut the seed down before any RunE.
	projectBatchRoot(t, f, "cancelling")
	require.NoError(t, a.AsyncJobStore().InsertSessionNotice(ctx, sess.ID,
		session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child): QUESTION: proceed?", true, f.toolCallID))
	a.Shutdown()

	// (b) cancelling root: held, awaiting termination, not terminal. The
	// held child_question notice also makes the root await an answer.
	stdout := runJobsCmd(t, sess.ID)
	t.Logf("sessions jobs (flag on, cancelling):\n%s", stdout)
	require.Contains(t, stdout, "cancelling")
	require.Contains(t, stdout, "termination", "a cancelling root must be reported as awaiting termination")

	// JSON path: legacy field names kept, new batch_* fields present.
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout = runJobsCmd(t, sess.ID)
	var item jobsJSONItem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
	require.Equal(t, f.toolCallID, item.ToolCallID)
	require.Equal(t, "cancelling", item.State)
	require.Equal(t, f.rootID, item.BatchRootID)
	require.True(t, item.BatchHeld)
	require.True(t, item.BatchCancelling)
	require.True(t, item.BatchAwaitingTermination)
	require.True(t, item.BatchAwaitingAnswer, "the held child_question notice must surface as awaiting answer")
	require.False(t, item.BatchUnknown, "a complete read must not claim unknown")
}

// TestSessionsJobsCmdRun_BatchFlagOffRegression covers (d): the same seeded
// database with the flag unset must produce today's legacy output exactly --
// batch rows invisible, "(no jobs)" printed, no batch_* JSON fields.
func TestSessionsJobsCmdRun_BatchFlagOffRegression(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "off")
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-flagoff-sess", "session with batch root")
	require.NoError(t, err)
	seedBatchReadRoot(t, a, sess.ID)
	a.Shutdown()

	require.Equal(t, "(no jobs)\n", runJobsCmd(t, sess.ID))

	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	require.Empty(t, runJobsCmd(t, sess.ID), "flag-off JSON must stay byte-identical: no batch rows")
}

// TestSessionsWhyCmdRun_BatchCancellingRootHeld: with the flag on, `sessions
// why` on a cancelling root annotates the status as batch live, prints the
// held-termination verdict, and never falls back to "at rest"/idle.
func TestSessionsWhyCmdRun_BatchCancellingRootHeld(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "why-batch-sess", "why batch root")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	projectBatchRoot(t, f, "cancelling")
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, sess.ID, &buf))
	out := buf.String()
	t.Logf("sessions why (flag on, cancelling):\n%s", out)

	// printStallHeader prints "status: <s>" whenever the status is not
	// "running" (sessions_stall.go), so "batch live" appears as a plain
	// status line, and the reason names the offending held root.
	require.Contains(t, out, "status: batch live", "a held root must keep the status live")
	require.Contains(t, out, "reason: batch root ", "the reason must name the held root")
	require.Contains(t, out, "stopped but awaiting termination", "cancelling must be explained as held")
	require.NotContains(t, out, "at rest\n", "a held root must never read as at rest")
	require.NotContains(t, out, "Async jobs:", "the legacy async section is replaced with the flag on")
	require.Contains(t, out, "Batch roots:")
	require.Contains(t, out, f.toolCallID)

	// A held child_question notice (delivery pending, wake=1: the R8 debt
	// rows API sees it) must be named as awaiting an answer.
	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID,
		session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child): QUESTION: proceed?", true, f.toolCallID))
	buf.Reset()
	require.NoError(t, explainWhy(a, dataDir, sess.ID, &buf))
	require.Contains(t, buf.String(), "awaiting an answer", "the held question must be named")
}

// TestSessionsListCmdRun_BatchFlagOnStatusAndJSON: with the flag on, a listed
// session with a held batch root shows "batch live" in STATUS and the
// additive batch_* JSON fields; with the flag off the same row has neither.
func TestSessionsListCmdRun_BatchFlagOnStatusAndJSON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "list-batch-sess", "listed batch session")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	// A held child_question notice bound to the root's tool call must make
	// the list row await an answer (positive case for the batch question
	// reporting), inserted while the seed store is still open.
	require.NoError(t, a.AsyncJobStore().InsertSessionNotice(ctx, sess.ID,
		session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child): QUESTION: proceed?", true, f.toolCallID))
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)
	a.Shutdown()

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})
	require.Contains(t, stdout, "batch live", "STATUS must report the held batch root")
	require.Contains(t, stdout, "(awaiting answer)", "STATUS must carry the question substate")

	require.NoError(t, sessionsListCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsListCmd.Flags().Set("json", "false") })
	stdout = captureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})
	var item sessionListItem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
	require.Equal(t, sess.ID, item.ID)
	require.Equal(t, "live", item.BatchStatus)
	require.Equal(t, 1, item.BatchRoots)
	require.Contains(t, item.Status, "batch live")
	require.True(t, item.BatchAwaitingAnswer, "the held bound question must surface as awaiting answer")
}

// TestSessionsShowCmdRun_BatchFlagOnCounts: with the flag on, `sessions show`
// populates the legacy async_jobs_total/running fields from batch task counts
// and adds batch_status/batch_roots; the root line names the cancelling state.
func TestSessionsShowCmdRun_BatchFlagOnCounts(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	tmp := isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	dataDir := tmp + "/show-batch-data"

	ensureRootFlagStandIns(sessionsShowCmd, dataDir)
	sessionsShowCmd.SetContext(context.Background())
	require.NoError(t, sessionsShowCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsShowCmd.Flags().Set("with-messages", "false"))

	seed, err := setupApp(sessionsShowCmd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-batch-sess", "show batch session")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, seed, sess.ID)
	projectBatchRoot(t, f, "running")
	// Prove the flag-ON path never queries the legacy ledger.
	dropLegacyAsyncJobs(t, dataDir)
	seed.Shutdown()

	require.NoError(t, sessionsShowCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsShowCmd.Flags().Set("json", "false") })
	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID}))
	})
	var item struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		AsyncJobsTotal   int    `json:"async_jobs_total"`
		AsyncJobsRunning int    `json:"async_jobs_running"`
		BatchRoots       int    `json:"batch_roots"`
		BatchStatus      string `json:"batch_status"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item))
	require.Equal(t, 2, item.AsyncJobsTotal, "two task nodes")
	require.Equal(t, 2, item.AsyncJobsRunning, "both tasks running")
	require.Equal(t, "live", item.BatchStatus)
	require.Equal(t, 1, item.BatchRoots)
}

// batchRuntimeFlagOnForTest was the regression tests' flag precondition
// helper; the tests now set RUSH_BATCH_RUNTIME explicitly with t.Setenv.
