package sessions

// R12-B part 1 coverage: the session status commands read live batch node
// rows (real SQLite fixtures seeded through session.BatchStore.Reserve/
// Project, never :memory:) and report held roots, cancelling roots as
// "stopped but awaiting termination", and conservative UNKNOWN on
// incomplete/failed reads.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// ensureRootFlagStandIns and the command environments mirror the real-command
// fixtures from the original jobs/list tests, now using the shared cmdkit and
// cmdtest seams after the sessions package split.
func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if cmd.Flags().Lookup("debug") == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if cmd.Flags().Lookup("data-dir") == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dataDir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

func isolatedCommandEnv(t *testing.T, cmd *cobra.Command, dataName string) (a *app.App, workDir, dataDir string) {
	t.Helper()
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	workDir = t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	dataDir = filepath.Join(tmp, dataName)
	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(cmd, dataDir)
	if cmd.Flags().Lookup("json") == nil {
		cmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, cmd.Flags().Set("json", "false"))
	cmd.SetContext(ctx)

	built, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory)
	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})
	return built, workDir, dataDir
}

func isolatedJobsEnv(t *testing.T) (a *app.App, dataDir string) {
	a, _, dataDir = isolatedCommandEnv(t, sessionsJobsCmd, "jobs-data")
	return a, dataDir
}

func isolatedListEnvWithConfiguredDataDir(t *testing.T) (a *app.App, workDir, dataDir string) {
	return isolatedCommandEnv(t, sessionsListCmd, "configured-elsewhere-data")
}

func runJobsCmd(t *testing.T, args ...string) string {
	t.Helper()
	return cmdtest.CaptureStdout(t, func() {
		require.NoError(t, sessionsJobsCmd.RunE(sessionsJobsCmd, args))
	})
}

func runWhyCmd(t *testing.T, args ...string) string {
	t.Helper()
	return cmdtest.CaptureStdout(t, func() {
		require.NoError(t, sessionsWhyCmd.RunE(sessionsWhyCmd, args))
	})
}

// The command's JSON wire shape is intentionally copied from HEAD's
// sessions_jobs.go so assertions decode the production field names.
type jobsJSONItem struct {
	OwnerSessionID           string `json:"owner_session_id"`
	ToolCallID               string `json:"tool_call_id"`
	Kind                     string `json:"kind"`
	State                    string `json:"state"`
	Liveness                 string `json:"liveness,omitempty"`
	HostID                   string `json:"host_id"`
	HostPID                  int64  `json:"host_pid,omitempty"`
	HostLabel                string `json:"host_label,omitempty"`
	ChildSessionID           string `json:"child_session_id,omitempty"`
	Delivery                 string `json:"delivery"`
	Wake                     bool   `json:"wake"`
	Reacted                  bool   `json:"reacted"`
	CreatedAt                int64  `json:"created_at"`
	UpdatedAt                int64  `json:"updated_at"`
	ResultSummary            string `json:"result_summary,omitempty"`
	ResultIsError            bool   `json:"result_is_error,omitempty"`
	Hint                     string `json:"hint,omitempty"`
	BatchRootID              string `json:"batch_root_id,omitempty"`
	BatchHeld                bool   `json:"batch_held,omitempty"`
	BatchCancelling          bool   `json:"batch_cancelling,omitempty"`
	BatchAwaitingTermination bool   `json:"batch_awaiting_termination,omitempty"`
	BatchAwaitingAnswer      bool   `json:"batch_awaiting_answer,omitempty"`
	BatchDebt                int64  `json:"batch_debt,omitempty"`
	BatchTasksPending        int    `json:"batch_tasks_pending,omitempty"`
	BatchTasksActive         int    `json:"batch_tasks_active,omitempty"`
	BatchTasksDone           int    `json:"batch_tasks_done,omitempty"`
	BatchUnknown             bool   `json:"batch_unknown,omitempty"`
}

type sessionListItem struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	AwaitingAnswer      bool   `json:"awaiting_answer,omitempty"`
	AwaitingChild       string `json:"awaiting_child,omitempty"`
	AwaitingQuestion    string `json:"awaiting_question,omitempty"`
	BatchRoots          int    `json:"batch_roots,omitempty"`
	BatchStatus         string `json:"batch_status,omitempty"`
	BatchAwaitingAnswer bool   `json:"batch_awaiting_answer,omitempty"`
}

// batchReadFixture carries the seeded root's identities for assertions.
// closeAsync closes the seed's host store (releasing its host lock).
type batchReadFixture struct {
	rootID, nodeA, nodeB, host, owner, toolCallID string
	store                                         *session.BatchStore
	closeAsync                                    func()
}

// seedBatchReadRoot reserves a real batch root (kind=batch with tool call and
// input hash, two task children) for owner on a host registered by this
// live test process (sessionhosts.HostStore.EnsureHost), so the live reader
// proves liveness via the own-host registry. The host store is closed at
// cleanup so the host lock cannot block later command runs.
func seedBatchReadRoot(t *testing.T, a *app.App, owner string) batchReadFixture {
	t.Helper()
	ctx := context.Background()
	store := sessionhosts.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "batch-read-fixture")
	closeAsync := func() { _ = store.Close(context.Background()) }
	t.Cleanup(closeAsync)
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
	ctx := context.Background()
	reader := &session.BatchLiveReader{Source: errBatchLiveSource{err: context.DeadlineExceeded}}
	got := sesskit.ReduceBatchSessionFacts(ctx, reader, session.NewBatchStore(nil), nil, []string{"owner-x"})
	require.Contains(t, got, "owner-x")
	f := got["owner-x"]
	require.True(t, f.Unknown, "a failed read must be unknown")
	require.True(t, f.Live, "a failed read must never read as idle")
	require.Empty(t, f.Roots, "no facts may be invented from a failed read")

	// Nil reader: same conservative verdict.
	got = sesskit.ReduceBatchSessionFacts(ctx, nil, nil, nil, []string{"owner-y"})
	require.True(t, got["owner-y"].Unknown)
	require.True(t, got["owner-y"].Live)
	require.Equal(t, " (batch unknown)", sesskit.BatchStatusAnnotation(got["owner-y"]))
}

// TestSessionsJobsCmdRun_BatchLiveRoot covers (a) and (b): a pending root
// reports live and held, and a cancelling root reports as held/awaiting
// termination -- never terminal, never idle.
func TestSessionsJobsCmdRun_BatchLiveRoot(t *testing.T) {
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
	pending := sesskit.BatchRuntimeFacts(ctx, a, dataDir, []string{sess.ID})
	require.NotNil(t, pending)
	pf := pending[sess.ID]
	require.False(t, pf.Unknown, "the seed read must be complete")
	require.True(t, pf.Live, "a pending root is live")
	require.Len(t, pf.Roots, 1)
	require.Equal(t, f.toolCallID, pf.Roots[0].ToolCallID)
	require.Equal(t, "pending", pf.Roots[0].State)
	require.True(t, pf.Roots[0].Held)
	// Project to cancelling AND insert the held question while the seed
	// store is still open, then shut the seed down before any RunE.
	projectBatchRoot(t, f, "cancelling")
	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, sess.ID,
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

// seedSecondBatchRoot reserves a SECOND bare batch root (no children -- a
// bare root row satisfies the batch_nodes CHECKs) on the same host for a
// distinct tool call, so per-root question attribution can be proven across
// two live roots of one owner.
func seedSecondBatchRoot(t *testing.T, a *app.App, owner, host string) string {
	t.Helper()
	ctx := context.Background()
	reserved, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "batch-read-runner",
		ToolCallID: "other-root-call", InputHash: "other-root-hash", Sequence: 2,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	return reserved.Snapshot.Root.RootID
}

// TestSessionsJobsCmdRun_BoundQuestionStaysPerRoot: a held child_question
// notice bound to root A's tool call must mark ONLY root A as awaiting an
// answer -- in JSON (per-item batch_awaiting_answer) and in the text table
// (root A's AWAITING cell "answer", root B's "-").
//
// Static trace: two nonterminal roots on one owner are legal (the live-attach
// unique index is per (owner_session_id, tool_call_id), different tool calls,
// both live), both appear in BatchLiveForSessions' tree, and the bound query
// BatchListBoundNoticesByToolCallID matches only root A's job_tool_call_id,
// whose R11G-style notice row (delivery 'pending', wake 1, reacted 0)
// satisfies the bound predicate in reduceBatchSessionFacts
// (sessions_batch_read.go:184-185).
func TestSessionsJobsCmdRun_BoundQuestionStaysPerRoot(t *testing.T) {
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-two-root-sess", "two batch roots")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	seedSecondBatchRoot(t, a, f.owner, f.host)
	// One held child_question notice bound ONLY to root A's tool call.
	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, sess.ID,
		session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child): QUESTION: proceed?", true, f.toolCallID))
	a.Shutdown()

	// JSON path: two root items; only root A awaits an answer.
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout := runJobsCmd(t, sess.ID)
	var items []jobsJSONItem
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var it jobsJSONItem
		require.NoError(t, json.Unmarshal([]byte(line), &it), stdout)
		items = append(items, it)
	}
	require.Len(t, items, 2, "two live roots must render two rows")
	var itemA, itemB *jobsJSONItem
	for i := range items {
		switch items[i].ToolCallID {
		case f.toolCallID:
			itemA = &items[i]
		case "other-root-call":
			itemB = &items[i]
		}
	}
	require.NotNil(t, itemA, "root A's row must be present")
	require.NotNil(t, itemB, "root B's row must be present")
	require.True(t, itemA.BatchAwaitingAnswer, "the held bound question must surface on its own root")
	require.False(t, itemB.BatchAwaitingAnswer, "the question must NOT leak to the other root")
	require.True(t, itemA.BatchHeld, "both pending roots are held")
	require.True(t, itemB.BatchHeld, "both pending roots are held")
	require.False(t, itemA.BatchUnknown, "a complete read must not claim unknown")
	require.False(t, itemB.BatchUnknown, "a complete read must not claim unknown")

	// Text path: root A's AWAITING cell is "answer", root B's stays "-".
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "false"))
	stdout = runJobsCmd(t, sess.ID)
	var lineA, lineB string
	for _, ln := range strings.Split(stdout, "\n") {
		if strings.Contains(ln, f.toolCallID) {
			lineA = ln
		}
		if strings.Contains(ln, "other-root-call") {
			lineB = ln
		}
	}
	require.NotEmpty(t, lineA, "root A's table row must be present")
	require.NotEmpty(t, lineB, "root B's table row must be present")
	require.Contains(t, lineA, "answer", "root A's AWAITING cell must say answer")
	require.NotContains(t, lineB, "answer", "root B's AWAITING cell must stay '-'")
}

// TestSessionsJobsCmdRun_CompletionDebtNotAwaitingAnswer: done-but-unreacted
// completion debt on a task child (snapshot Debt>0, GuardDebt true) is NOT a
// question -- the row renders with batch_awaiting_answer false.
//
// Static trace: the fixture satisfies the batch_nodes CHECKs (root kind=
// 'batch' with tool_call_id/input_hash); the raw UPDATEs satisfy the raw
// CHECKs (root announced=1 WITH announce_message_id NOT NULL; task
// notice_selected=1 with kind/state/cause NOT NULL, sequence>0, ordinal>=0,
// delivery='done'). The debt predicate (batchHistoryDebtRow: selected, wake=1,
// reacted=0, delivery != 'void') is satisfied; the debt-rows kind fallback
// (sessions_batch_read.go:210-214) fires only on Kind==child_question, which
// this row is not, so HasQuestion stays false.
func TestSessionsJobsCmdRun_CompletionDebtNotAwaitingAnswer(t *testing.T) {
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-debt-sess", "completion debt session")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	// Root stays PENDING (live, renders a row); seed ordinary completion debt
	// on ONE task child while the seed app is alive.
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET announced = 1, announce_message_id = 'debt-msg' WHERE node_id = ? AND root_id = ?`,
		f.rootID, f.rootID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'task', notice_state = 'completed',
		 notice_cause = '', notice_sequence = 1, notice_ordinal = 0, delivery = 'done', wake = 1
		 WHERE node_id = ? AND parent_id = ?`, f.nodeA, f.rootID)
	require.NoError(t, err)
	a.Shutdown()

	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout := runJobsCmd(t, sess.ID)
	var item jobsJSONItem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
	require.Equal(t, f.toolCallID, item.ToolCallID)
	require.False(t, item.BatchAwaitingAnswer, "completion debt is not a question")
	require.GreaterOrEqual(t, item.BatchDebt, int64(1), "the done-but-unreacted notice counts as debt")
	require.False(t, item.BatchUnknown, "a complete read must not claim unknown")
}

// TestSessionsWhyCmdRun_BatchCancellingRootHeld: `sessions why` on a
// cancelling root annotates the status as batch live, prints the
// held-termination verdict, and never falls back to "at rest"/idle.
func TestSessionsWhyCmdRun_BatchCancellingRootHeld(t *testing.T) {
	a, _, dataDir := isolatedCommandEnv(t, sessionsWhyCmd, "why-batch-data")
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "why-batch-sess", "why batch root")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	projectBatchRoot(t, f, "cancelling")

	a.Shutdown()
	out := runWhyCmd(t, sess.ID)
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
	conn, err := cmdtest.DBConnectForTest(t, dataDir)
	require.NoError(t, err)
	require.NoError(t, session.NewSessionNoticeStore(conn).InsertSessionNotice(ctx, sess.ID,
		session.NoticeKindChildQuestion, "SUB-AGENT QUESTION (session child): QUESTION: proceed?", true, f.toolCallID))
	out = runWhyCmd(t, sess.ID)
	require.Contains(t, out, "awaiting an answer", "the held question must be named")
}

// TestSessionsListCmdRun_BatchFlagOnStatusAndJSON: a listed session with a
// held batch root shows "batch live" in STATUS and the additive batch_* JSON
// fields.
func TestSessionsListCmdRun_BatchStatusAndJSON(t *testing.T) {
	a, _, _ := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "list-batch-sess", "listed batch session")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	child, err := a.Sessions.CreateTaskSession(ctx, "list-batch-child", sess.ID, "worker")
	require.NoError(t, err)
	updates := []db.ProjectBatchNodeParams{{NodeID: f.nodeA, State: "running", ControlState: "none", Token: 7}}
	require.NoError(t, f.store.Project(ctx, session.BatchProjection{
		Host: f.host, RunnerID: "batch-read-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: sess.ID, Root: f.rootID, Updates: updates}},
	}))
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n", f.nodeA, 7, 3, 1, child.ID)
	text := marker + "SUB-AGENT QUESTION: proceed?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.NoError(t, f.store.BatchPublishQuestion(ctx, sess.ID, f.nodeA, 7, marker, text))
	a.Shutdown()

	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})
	require.Contains(t, stdout, "batch live", "STATUS must report the held batch root")
	require.Contains(t, stdout, "(awaiting answer)", "STATUS must carry the question substate")

	require.NoError(t, sessionsListCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsListCmd.Flags().Set("json", "false") })
	stdout = cmdtest.CaptureStdout(t, func() {
		require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
	})
	var item sessionListItem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
	require.Equal(t, sess.ID, item.ID)
	require.Equal(t, "live", item.BatchStatus)
	require.Equal(t, 1, item.BatchRoots)
	require.Contains(t, item.Status, "batch live")
	require.True(t, item.BatchAwaitingAnswer, "the held bound question must surface as awaiting answer")
	require.True(t, item.AwaitingAnswer)
	require.Equal(t, child.ID, item.AwaitingChild)
	require.Contains(t, item.AwaitingQuestion, "proceed?")
}

// TestSessionsShowCmdRun_BatchCounts: `sessions show` populates the
// batch_tasks_total/running fields from batch task counts and adds
// batch_status/batch_roots.
func TestSessionsShowCmdRun_BatchCounts(t *testing.T) {
	seed, _, dataDir := isolatedCommandEnv(t, sessionsShowCmd, "show-batch-data")
	require.NoError(t, sessionsShowCmd.Flags().Set("with-messages", "false"))

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-batch-sess", "show batch session")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, seed, sess.ID)
	projectBatchRoot(t, f, "running")
	seed.Shutdown()
	conn, err := cmdtest.DBConnectForTest(t, dataDir)
	require.NoError(t, err)
	var seededNodes int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id = ?`, f.rootID).Scan(&seededNodes))
	require.Equal(t, 3, seededNodes, "one root and two task rows were seeded")

	require.NoError(t, sessionsShowCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsShowCmd.Flags().Set("json", "false") })
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID}))
	})
	var item struct {
		ID                string `json:"id"`
		Status            string `json:"status"`
		BatchTasksTotal   int    `json:"batch_tasks_total"`
		BatchTasksRunning int    `json:"batch_tasks_running"`
		BatchRoots        int    `json:"batch_roots"`
		BatchStatus       string `json:"batch_status"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item))
	require.Equal(t, 2, item.BatchTasksTotal, "two task nodes")
	require.Equal(t, 2, item.BatchTasksRunning, "both tasks running")
	require.Equal(t, "live", item.BatchStatus)
	require.Equal(t, 1, item.BatchRoots)
}
