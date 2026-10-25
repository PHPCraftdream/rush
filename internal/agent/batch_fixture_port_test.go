// Test-only batch fixture helpers (batch-only since the ledger deletion).
// Node-facts usage: bindBatchNodeFacts(t, coord, conn, dataDir). Seed job
// debt with seedFixtureJobDebt(t, conn, owner, callID, pulled); read it with
// readFixtureJobDebt. failFixtureReaction, failFixtureSettle, and
// failFixturePull install batch-store reaction/pull blockers. The round-trip
// self-tests live in batch_fixture_port_selftest_test.go.
// Final helper list:
//
//	bindBatchNodeFacts(t *testing.T, c *coordinator, conn *sql.DB, dataDir string) BatchCutoverFacts
//	bindBatchCoordinatorControls(t *testing.T, c *coordinator, conn *sql.DB, dataDir string, ingress BatchWorkIngress)
//	seedBatchJobDebt(t *testing.T, conn *sql.DB, owner, callID string, visible bool) (root, child string)
//	seedBatchDelegation(t *testing.T, conn *sql.DB, owner, callID, childID, kind, state string, withNotice bool) (root, child string)
//	batchJobAsyncJobReadback(t *testing.T, conn *sql.DB, owner, rootID, nodeID string) fixtureAsyncJobRow
//	seedFixtureJobDebt(t *testing.T, conn *sql.DB, owner, callID string, pulled bool) (root, child string)
//	seedFixtureJobDebtStoreOnly(t *testing.T, conn *sql.DB, owner, callID string, pulled bool)
//	seedFixtureDelegation(t *testing.T, conn *sql.DB, owner, callID, childID, kind, state string, withNotice bool) (root, child string)
//	readFixtureJobDebt(t *testing.T, conn *sql.DB, owner, callID string) fixtureAsyncJobRow
//	readFixtureDelegation(t *testing.T, conn *sql.DB, owner, callID string) batchDelegationView
//	type batchDelegationView struct { Root, Node, ChildSessionID, Kind, State, Delivery string; NoticeSelected, Announced, Reacted, Wake int64; ScopeOpen bool }
//	failFixtureReaction(t *testing.T, conn *sql.DB) / failFixtureSettle(...) / failFixturePull(...)
//	failBatchNodeReaction(t *testing.T, conn *sql.DB, settle bool) / failBatchRetainedNoticeReaction(...) / failBatchPull(...)
//
// Standalone fixtures need an explicit binding (see
// coordinator_recheck_child_test.go's newBlockingWakeCoordinator and
// supervision_test.go's newSupervisionTestCoordinator). Never auto-bind every
// &coordinator{}: missing-source tests must remain unbound. AwaitTasks also
// needs an explicit live/control binding, separate from node facts.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func batchFixtureLiveReader(conn *sql.DB, dataDir string) *session.BatchLiveReader {
	live := session.NewBatchLiveReader(db.New(conn), dataDir)
	live.TxStarter = conn
	live.ProbeHost = func(dir, host string) (session.HostLockStatus, error) {
		if host == "fixture-host" {
			return session.HostStatusAlive, nil
		}
		return session.ProbeHostShared(dir, host)
	}
	return live
}

// bindBatchNodeFacts binds actual node facts using the production shared-writer
// shape. It never mutates any environment variable and cleanup removes only c.
func bindBatchNodeFacts(t *testing.T, c *coordinator, conn *sql.DB, dataDir string) BatchCutoverFacts {
	t.Helper()
	live := batchFixtureLiveReader(conn, dataDir)
	facts := NewBatchCutoverNodeFacts(session.NewBatchStore(conn), live, conn)
	BindCoordinatorBatchCutoverFacts(c, facts)
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(c) })
	return facts
}

// bindBatchCoordinatorControls explicitly supplies AwaitTasks its live reader;
// it is independent of the cutover-facts binding. Cleanup is coordinator-local.
func bindBatchCoordinatorControls(t *testing.T, c *coordinator, conn *sql.DB, dataDir string, ingress BatchWorkIngress) {
	t.Helper()
	live := batchFixtureLiveReader(conn, dataDir)
	require.NoError(t, BindCoordinatorBatchControls(c, ingress, nil, live))
	t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
}

func batchFixtureMessage(text string) message.CreateMessageParams {
	return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}, BackgroundJobNotice: true}
}

// seedBatchJobDebt reserves a task, then validly projects both root and child
// terminal at sequence 2. Only the selected child completion is debt; the root
// has no selected notice. Actual start/pull APIs assign history identities.
func seedBatchJobDebt(t *testing.T, conn *sql.DB, owner, callID string, visible bool) (root, child string) {
	t.Helper()
	ctx := context.Background()
	ensureFixtureSession(t, conn, owner)
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, child = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	require.NoError(t, store.Project(ctx, session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: owner, Root: root,
			Updates: []db.ProjectBatchNodeParams{
				{NodeID: root, State: "completed", OutcomeState: "completed", OutcomeSummary: "output", ControlState: "none"},
				{NodeID: child, State: "completed", OutcomeState: "completed", OutcomeSummary: "output", ControlState: "none"},
			},
			Notices: []db.SelectBatchNodeNoticeParams{{
				NodeID:     child,
				NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
				NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
			}},
		}},
	}))
	start, err := store.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, root, batchFixtureMessage("fixture started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	if visible {
		pulled, err := store.BatchPullNotices(ctx, message.NewService(db.New(conn)), owner, func(row session.BatchNoticeRow) message.CreateMessageParams {
			return batchFixtureMessage("fixture output: " + row.OutcomeSummary)
		})
		require.NoError(t, err)
		found := false
		for _, row := range pulled {
			if row.NodeID == child {
				found = true
				break
			}
		}
		require.True(t, found, "the seeded child completion must be pulled")
	}
	return root, child
}

// seedBatchDelegation reserves and announces the real parent/child task link.
// Live states stay live/unselected; released states project parent AND child to
// the same terminal state. No completion notice is synthesized UNLESS withNotice
// is set (terminal states only): then the SAME Project call also selects a
// child completion notice (mirroring seedBatchJobDebt), left delivery=pending.
// Ordering is safe: BatchAnnounceStarted runs BEFORE Project, so the root's
// announced=1 that BatchOutboxDebtNodes requires already exists when the notice
// is selected.
func seedBatchDelegation(t *testing.T, conn *sql.DB, owner, callID, childID, kind, state string, withNotice bool) (root, child string) {
	t.Helper()
	ctx := context.Background()
	ensureFixtureSession(t, conn, owner)
	store := session.NewBatchStore(conn)
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: "fixture-host", RunnerID: "fixture-runner", ToolCallID: callID,
		InputHash: "fixture-hash-" + callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: kind, Valid: true}, ChildSessionID: sql.NullString{String: childID, Valid: childID != ""}, State: state, ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, child = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, message.NewService(db.New(conn)), owner, root, batchFixtureMessage("fixture delegation started"))
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	if state != "completed" && state != "failed" && state != "cancelled" && state != "timed_out" && state != "interrupted" && state != "skipped" {
		if state != "pending" && state != "starting" && state != "running" && state != "cancelling" {
			t.Fatalf("unsupported batch delegation state %q", state)
		}
		return root, child
	}
	proj := session.BatchProjection{
		Host: "fixture-host", RunnerID: "fixture-runner", Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{
			{NodeID: root, State: state, OutcomeState: state, OutcomeSummary: "fixture delegation released", ControlState: "none"},
			{NodeID: child, State: state, OutcomeState: state, OutcomeSummary: "fixture delegation released", ControlState: "none"},
		}}},
	}
	if withNotice {
		proj.Roots[0].Notices = []db.SelectBatchNodeNoticeParams{{
			NodeID:     child,
			NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: state, Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
			NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
		}}
	}
	require.NoError(t, store.Project(ctx, proj))
	return root, child
}

// fixtureAsyncJobRow is the retained legacy async_jobs readback shape for the
// tests that still speak the deleted db.AsyncJob vocabulary. Field-for-field
// identical to the deleted type; in the batch world ClaimID is always empty
// (batch chains key on the root's tool_call_id, never on claim ids).
type fixtureAsyncJobRow struct {
	OwnerSessionID    string
	ToolCallID        string
	Kind              string
	ToolName          string
	TimeoutSeconds    int64
	InputHash         string
	ChildSessionID    sql.NullString
	OriginCli         int64
	State             string
	NoticeKind        string
	HostID            string
	Announced         int64
	Delivery          string
	NoticeMessageID   sql.NullString
	Wake              int64
	Reacted           int64
	WakeAttempts      int64
	ReactedFailed     int64
	DeadlineAt        sql.NullInt64
	TimeoutKind       sql.NullString
	ResultSummary     sql.NullString
	ResultIsError     sql.NullInt64
	CreatedAt         int64
	UpdatedAt         int64
	ClaimID           string
	AnnounceMessageID sql.NullString
}

// batchJobAsyncJobReadback is an explicit test-only compatibility mapping for
// legacy-shaped callers. It reads the selected CHILD node and maps task_kind to
// Kind/ToolName. The actual root call/announcement fields remain root facts.
func batchJobAsyncJobReadback(t *testing.T, conn *sql.DB, owner, rootID, nodeID string) fixtureAsyncJobRow {
	t.Helper()
	snap, err := session.NewBatchStore(conn).Read(context.Background(), owner, rootID)
	require.NoError(t, err)
	var root, child db.BatchNode
	for _, n := range snap.Nodes {
		if n.NodeID == rootID {
			root = n
		}
		if n.NodeID == nodeID {
			child = n
		}
	}
	require.Equal(t, rootID, root.NodeID)
	require.Equal(t, rootID, child.RootID)
	require.EqualValues(t, 1, child.NoticeSelected)
	kind := child.TaskKind.String
	return fixtureAsyncJobRow{
		OwnerSessionID: owner, ToolCallID: root.ToolCallID.String, Kind: kind, ToolName: kind,
		ChildSessionID: child.ChildSessionID, OriginCli: root.OriginCli, State: child.State,
		HostID: root.HostID, Announced: root.Announced, Delivery: child.Delivery,
		NoticeMessageID: child.NoticeMessageID, Wake: child.Wake, Reacted: child.Reacted,
		WakeAttempts: child.WakeAttempts, ReactedFailed: child.ReactedFailed,
		CreatedAt: child.CreatedAt, UpdatedAt: child.UpdatedAt, AnnounceMessageID: root.AnnounceMessageID,
		ResultSummary: sql.NullString{String: child.OutcomeSummary, Valid: child.OutcomeSummary != ""},
	}
}

// seedFixtureJobDebt seeds terminal job debt through the batch store (the
// reservation/project/announce/pull seed). The legacy store/ledger dual-mode
// half is gone with the work ledger.
func seedFixtureJobDebt(t *testing.T, conn *sql.DB, owner, callID string, pulled bool) (root, child string) {
	t.Helper()
	return seedBatchJobDebt(t, conn, owner, callID, pulled)
}

// seedFixtureJobDebtStoreOnly keeps its name for the former cross-process
// distinction: in the batch world the batch store IS the only authority, so
// this is exactly seedBatchJobDebt.
func seedFixtureJobDebtStoreOnly(t *testing.T, conn *sql.DB, owner, callID string, pulled bool) {
	t.Helper()
	seedBatchJobDebt(t, conn, owner, callID, pulled)
}

// seedFixtureDelegation seeds a delegation through the batch store. kind is
// the batch task kind (e.g. "command"); withNotice
// (terminal states only) selects a completion notice WITHOUT pulling it,
// left delivery=pending.
func seedFixtureDelegation(t *testing.T, conn *sql.DB, owner, callID, childID, kind, state string, withNotice bool) (root, child string) {
	t.Helper()
	return seedBatchDelegation(t, conn, owner, callID, childID, kind, state, withNotice)
}

// readFixtureJobDebt loads the seeded job's selected CHILD node through the
// batch store, mapped to the legacy db.AsyncJob shape for retained callers.
func readFixtureJobDebt(t *testing.T, conn *sql.DB, owner, callID string) fixtureAsyncJobRow {
	t.Helper()
	var rootID string
	err := conn.QueryRowContext(context.Background(), `SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`, owner, callID).Scan(&rootID)
	require.NoError(t, err)
	snap, err := session.NewBatchStore(conn).Read(context.Background(), owner, rootID)
	require.NoError(t, err)
	var nodeID string
	for _, node := range snap.Nodes {
		if node.NodeID != rootID && node.NoticeSelected == 1 {
			nodeID = node.NodeID
			break
		}
	}
	require.NotEmpty(t, nodeID)
	return batchJobAsyncJobReadback(t, conn, owner, rootID, nodeID)
}

// batchDelegationView is the batch delegation readback. Root/Node are batch
// node ids. Announced comes from the ROOT row (batch_outbox.sql updates
// announced on node_id=root_id only); NoticeSelected/Reacted and the notice
// state are child-row facts, as are Wake/Delivery. ScopeOpen is the CHILD
// scope's open state -- the child's OWN liveness in batch terms: the child
// link is live (pending/starting/running/cancelling) OR the child owes a
// reaction (a selected, unreacted notice). An unnamed child has no scope to
// keep open and always reports false.
type batchDelegationView struct {
	Root, Node, ChildSessionID, Kind, State, Delivery string
	NoticeSelected, Announced, Reacted, Wake          int64
	ScopeOpen                                         bool
}

// readFixtureDelegation reads one seeded delegation through the batch store.
func readFixtureDelegation(t *testing.T, conn *sql.DB, owner, callID string) batchDelegationView {
	t.Helper()
	var rootID string
	err := conn.QueryRowContext(context.Background(), `SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`, owner, callID).Scan(&rootID)
	require.NoError(t, err)
	snap, err := session.NewBatchStore(conn).Read(context.Background(), owner, rootID)
	require.NoError(t, err)
	var child db.BatchNode
	var rootRow db.BatchNode
	found := false
	for _, node := range snap.Nodes {
		if node.NodeID == rootID {
			rootRow = node
			continue
		}
		if !found {
			child, found = node, true
		}
	}
	require.True(t, found, "fixture delegation root must have a child node")
	require.Equal(t, rootID, rootRow.NodeID, "fixture delegation snapshot must contain its root row")
	view := batchDelegationView{
		Root: rootID, Node: child.NodeID, ChildSessionID: child.ChildSessionID.String,
		Kind: child.TaskKind.String, State: child.State, Delivery: child.Delivery,
		NoticeSelected: child.NoticeSelected, Announced: rootRow.Announced, Reacted: child.Reacted, Wake: child.Wake,
		ScopeOpen: false,
	}
	live := child.State == "pending" || child.State == "starting" || child.State == "running" || child.State == "cancelling"
	owesReaction := child.NoticeSelected == 1 && child.Reacted == 0
	view.ScopeOpen = view.ChildSessionID != "" && (live || owesReaction)
	return view
}

// failFixtureReaction blocks ONLY the ordinary batch reaction write
// (reacted=1, reacted_failed=0), so a settle-by-failure still goes through.
func failFixtureReaction(t *testing.T, conn *sql.DB) {
	t.Helper()
	failBatchNodeReaction(t, conn, false)
}

// failFixtureSettle blocks EVERY batch reaction write (reacted=1), ordinary
// AND settle-by-failure alike; byte-parity with the former inline
// async_jobs injectors.
func failFixtureSettle(t *testing.T, conn *sql.DB) {
	t.Helper()
	failBatchNodeReaction(t, conn, true)
	failBatchRetainedNoticeReaction(t, conn)
}

// failFixturePull fails a delivery-history insert.
func failFixturePull(t *testing.T, conn *sql.DB) {
	t.Helper()
	failBatchPull(t, conn)
}

// failBatchNodeReaction blocks batch-node reaction accounting. settle=false
// targets ONLY the ordinary reaction write (reacted=1, reacted_failed=0), so a
// settle-by-failure still goes through; settle=true blocks EVERY reaction
// write (WHEN NEW.reacted=1), ordinary AND settle-by-failure alike.
func failBatchNodeReaction(t *testing.T, conn *sql.DB, settle bool) {
	t.Helper()
	condition := "NEW.reacted=1 AND NEW.reacted_failed=0"
	if settle {
		condition = "NEW.reacted=1"
	}
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_batch_node_reaction BEFORE UPDATE OF reacted ON batch_nodes WHEN `+condition+` BEGIN SELECT RAISE(ABORT, 'fx: batch node reaction blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_batch_node_reaction`) })
}

func failBatchRetainedNoticeReaction(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_batch_notice_reaction BEFORE UPDATE OF reacted ON session_notices WHEN NEW.reacted=1 BEGIN SELECT RAISE(ABORT, 'fx: retained notice reaction blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_batch_notice_reaction`)
	})
}

func failBatchPull(t *testing.T, conn *sql.DB) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_batch_pull BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'fx: batch pull blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_batch_pull`) })
}
