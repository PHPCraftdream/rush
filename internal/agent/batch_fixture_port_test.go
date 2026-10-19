// Test-only batch fixture helpers. Node-facts usage: bindBatchNodeFacts(t,
// coord, conn, dataDir). Seed job debt with seedFixtureJobDebt(t, conn,
// legacyStore, ledger, messages, owner, callID, pulled); read it with
// readFixtureJobDebt. failFixtureReaction, failFixtureSettle, and
// failFixturePull target the RUSH_BATCH_RUNTIME-selected authority. The
// round-trip self-tests live in batch_fixture_port_selftest_test.go.
// Final helper list:
//
//	bindBatchNodeFacts(t *testing.T, c *coordinator, conn *sql.DB, dataDir string) BatchCutoverFacts
//	bindBatchCoordinatorControls(t *testing.T, c *coordinator, conn *sql.DB, dataDir string, ingress BatchWorkIngress)
//	seedBatchJobDebt(t *testing.T, conn *sql.DB, owner, callID string, visible bool) (root, child string)
//	seedBatchDelegation(t *testing.T, conn *sql.DB, owner, callID, childID, kind, state string, withNotice bool) (root, child string)
//	batchJobAsyncJobReadback(t *testing.T, conn *sql.DB, owner, rootID, nodeID string) db.AsyncJob
//	seedFixtureJobDebt(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, ledger *workLedger, messages message.Service, owner, callID string, pulled bool) (root, child string)
//	seedFixtureJobDebtStoreOnly(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, messages message.Service, owner, callID string, pulled bool)
//	seedFixtureDelegation(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID, childID string, kind session.JobKind, toolName, state string, withNotice bool) (root, child string)
//	readFixtureJobDebt(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID string) db.AsyncJob
//	readFixtureDelegation(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, ledger *workLedger, owner, callID string) batchDelegationView
//	type batchDelegationView struct { Root, Node, ChildSessionID, Kind, State, Delivery string; NoticeSelected, Announced, Reacted, Wake int64; ScopeOpen bool }
//	failFixtureReaction(t *testing.T, conn *sql.DB) / failFixtureSettle(...) / failFixturePull(...)
//	failBatchNodeReaction(t *testing.T, conn *sql.DB, settle bool) / failBatchRetainedNoticeReaction(...) / failBatchPull(...)
//
// newAttemptFixture and newChildPolicyTestCoordinator bind automatically;
// standalone fixtures need an explicit binding. See work_ledger_delegation_test.go
// (newParkedOutcomeCoordinator*), coordinator_recheck_child_test.go
// (newBlockingWakeCoordinator), supervision_test.go (newSupervisionTestLedger),
// bare autoresume/allowlist coordinator literals, and ledger-only SessionAgent
// fixtures that first attach a coordinator. Never auto-bind every &coordinator{}:
// missing-source tests must remain unbound. AwaitTasks also needs an explicit
// live/control binding, separate from node facts.
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
// shape. It never mutates RUSH_BATCH_RUNTIME and cleanup removes only c.
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

// batchJobAsyncJobReadback is an explicit test-only compatibility mapping for
// legacy-shaped callers. It reads the selected CHILD node and maps task_kind to
// Kind/ToolName. The actual root call/announcement fields remain root facts.
func batchJobAsyncJobReadback(t *testing.T, conn *sql.DB, owner, rootID, nodeID string) db.AsyncJob {
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
	return db.AsyncJob{
		OwnerSessionID: owner, ToolCallID: root.ToolCallID.String, Kind: kind, ToolName: kind,
		ChildSessionID: child.ChildSessionID, OriginCli: root.OriginCli, State: child.State,
		HostID: root.HostID, Announced: root.Announced, Delivery: child.Delivery,
		NoticeMessageID: child.NoticeMessageID, Wake: child.Wake, Reacted: child.Reacted,
		WakeAttempts: child.WakeAttempts, ReactedFailed: child.ReactedFailed,
		CreatedAt: child.CreatedAt, UpdatedAt: child.UpdatedAt, AnnounceMessageID: root.AnnounceMessageID,
		ResultSummary: sql.NullString{String: child.OutcomeSummary, Valid: child.OutcomeSummary != ""},
	}
}

// seedFixtureJobDebt is the shared dual-mode setup: flag ON delegates to the
// batch reservation/project/announce/pull seed; OFF retains the workLedger and
// AsyncJobStore claim path.
func seedFixtureJobDebt(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, ledger *workLedger, messages message.Service, owner, callID string, pulled bool) (root, child string) {
	t.Helper()
	if batchFlagEnabled() {
		return seedBatchJobDebt(t, conn, owner, callID, pulled)
	}
	_, existing, err := ledger.Start(owner, callID, callID, "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	require.False(t, existing)
	require.NoError(t, legacy.MarkAnnounced(context.Background(), owner, callID))
	_, err = legacy.Transition(context.Background(), session.TransitionParams{Owner: owner, ToolCallID: callID, State: "completed", ResultSummary: "output", Wake: true})
	require.NoError(t, err)
	if pulled {
		_, err = legacy.PullJobNotices(context.Background(), messages, owner, buildJobNoticeMessageParams)
		require.NoError(t, err)
	}
	return "", ""
}

// seedFixtureJobDebtStoreOnly is the cross-process-scope OFF variant: it
// produces EXACTLY the pre-round-1 direct-store state -- a terminal async_jobs
// row with unreacted pulled debt and NOTHING in the in-memory workLedger map.
// ON mode has no such distinction (the batch store is the only authority), so
// it delegates to seedBatchJobDebt unchanged.
func seedFixtureJobDebtStoreOnly(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, messages message.Service, owner, callID string, pulled bool) {
	t.Helper()
	if batchFlagEnabled() {
		seedBatchJobDebt(t, conn, owner, callID, pulled)
		return
	}
	ctx := context.Background()
	_, err := legacy.Claim(ctx, session.ClaimParams{Owner: owner, ToolCallID: callID, Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, legacy.MarkAnnounced(ctx, owner, callID))
	_, err = legacy.Transition(ctx, session.TransitionParams{Owner: owner, ToolCallID: callID, State: "completed", ResultSummary: "output", Wake: true})
	require.NoError(t, err)
	if pulled {
		_, err = legacy.PullJobNotices(ctx, messages, owner, buildJobNoticeMessageParams)
		require.NoError(t, err)
	}
}

// seedFixtureDelegation is the dual-mode delegation seed used by fixtures:
// batch mode creates a node child-link; OFF mode claims, announces and settles
// the legacy delegation row. Returns (root, child); both are empty in legacy
// mode. withNotice (terminal states only) selects a completion notice WITHOUT
// pulling it: ON via the Project Notices list, OFF via Transition NoticeKind
// (the legacy NoticeKind is a free-form string; job tests use "completed",
// the constant the job-done path itself carries), keeping Wake=true and the
// row's default delivery=pending.
func seedFixtureDelegation(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID, childID string, kind session.JobKind, toolName, state string, withNotice bool) (root, child string) {
	t.Helper()
	if batchFlagEnabled() {
		return seedBatchDelegation(t, conn, owner, callID, childID, string(kind), state, withNotice)
	}
	ctx := context.Background()
	_, err := legacy.Claim(ctx, session.ClaimParams{Owner: owner, ToolCallID: callID, Kind: kind, Input: "fixture input", ChildSessionID: childID, ToolName: toolName})
	require.NoError(t, err)
	if state == "pending" || state == "starting" || state == "running" || state == "cancelling" {
		require.NoError(t, legacy.MarkAnnounced(ctx, owner, callID))
		return "", ""
	}
	if state != "completed" && state != "failed" && state != "cancelled" && state != "timed_out" && state != "interrupted" && state != "skipped" {
		t.Fatalf("unsupported delegation state %q", state)
	}
	require.NoError(t, legacy.MarkAnnounced(ctx, owner, callID))
	noticeKind := ""
	if withNotice {
		noticeKind = "completed"
	}
	_, err = legacy.Transition(ctx, session.TransitionParams{Owner: owner, ToolCallID: callID, State: state, ResultSummary: "fixture delegation released", NoticeKind: noticeKind, Wake: true})
	require.NoError(t, err)
	return "", ""
}

// readFixtureJobDebt returns the selected-authority compatibility view. Batch
// mode selects root_id from terminal-inclusive root rows, then loads the real
// selected child; OFF queries the legacy store.
func readFixtureJobDebt(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, owner, callID string) db.AsyncJob {
	t.Helper()
	if batchFlagEnabled() {
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
	row, err := legacy.Get(context.Background(), owner, callID)
	require.NoError(t, err)
	return row
}

// batchDelegationView is the dual-mode delegation readback. Root/Node are
// batch node ids and empty in OFF mode; NoticeSelected maps to the batch
// node's selected-notice flag in ON mode and honestly to "the legacy row
// carries a NoticeKind" in OFF mode (the legacy row has no selected flag).
// Delivery/Wake round-trip the child's notice delivery state (ON: child node
// columns; OFF: the legacy row's own columns).
// ScopeOpen is the CHILD scope's open state -- the child's OWN work/debt per
// work_ledger_delegation.go (childScopeDrained :225 running check,
// :239-245 driver/busy gates, :277 childScopeOpenAcrossProcesses / CLIScope,
// :282 Drain==DrainOwed) -- NOT the delegation's own liveness: a live
// delegation on an empty child reports ScopeOpen=false, and a terminal
// delegation whose child still owes a reaction reports true.
type batchDelegationView struct {
	Root, Node, ChildSessionID, Kind, State, Delivery string
	NoticeSelected, Announced, Reacted, Wake          int64
	ScopeOpen                                         bool
}

// readFixtureDelegation reads one seeded delegation through the
// RUSH_BATCH_RUNTIME-selected authority. ScopeOpen is
// !ledger.childScopeDrained(childSessionID); an empty child id maps to false
// in BOTH modes (an unnamed child has no scope to keep open).
func readFixtureDelegation(t *testing.T, conn *sql.DB, legacy *session.AsyncJobStore, ledger *workLedger, owner, callID string) batchDelegationView {
	t.Helper()
	if batchFlagEnabled() {
		var rootID string
		err := conn.QueryRowContext(context.Background(), `SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`, owner, callID).Scan(&rootID)
		require.NoError(t, err)
		snap, err := session.NewBatchStore(conn).Read(context.Background(), owner, rootID)
		require.NoError(t, err)
		var child db.BatchNode
		found := false
		for _, node := range snap.Nodes {
			if node.NodeID == rootID {
				continue
			}
			child, found = node, true
			break
		}
		require.True(t, found, "fixture delegation root must have a child node")
		// Announced comes from the ROOT row: batch_outbox.sql:27-28
		// (BatchOutboxAnnounce) updates announced on node_id=root_id only, so
		// the child's announced flag stays 0 forever. NoticeSelected/Reacted
		// and the notice state (SelectBatchNodeNotice / SelectBatchNodeReact)
		// are child-row facts, as are Wake/Delivery.
		var rootRow db.BatchNode
		for _, node := range snap.Nodes {
			if node.NodeID == rootID {
				rootRow = node
				break
			}
		}
		require.Equal(t, rootID, rootRow.NodeID, "fixture delegation snapshot must contain its root row")
		view := batchDelegationView{
			Root: rootID, Node: child.NodeID, ChildSessionID: child.ChildSessionID.String,
			Kind: child.TaskKind.String, State: child.State, Delivery: child.Delivery,
			NoticeSelected: child.NoticeSelected, Announced: rootRow.Announced, Reacted: child.Reacted, Wake: child.Wake,
			ScopeOpen: false,
		}
		if child.ChildSessionID.String != "" {
			view.ScopeOpen = !ledger.childScopeDrained(child.ChildSessionID.String)
		}
		return view
	}
	row, err := legacy.Get(context.Background(), owner, callID)
	require.NoError(t, err)
	view := batchDelegationView{
		ChildSessionID: row.ChildSessionID.String,
		Kind:           row.Kind, State: row.State, Delivery: row.Delivery,
		NoticeSelected: 0, Announced: row.Announced, Reacted: row.Reacted, Wake: row.Wake,
		ScopeOpen: false,
	}
	if row.NoticeKind != "" {
		view.NoticeSelected = 1
	}
	if row.ChildSessionID.String != "" {
		view.ScopeOpen = !ledger.childScopeDrained(row.ChildSessionID.String)
	}
	return view
}

// failFixtureReaction targets batch_nodes in ON mode and async_jobs in OFF mode.
func failFixtureReaction(t *testing.T, conn *sql.DB) {
	t.Helper()
	if batchFlagEnabled() {
		failBatchNodeReaction(t, conn, false)
		return
	}
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_legacy_reaction BEFORE UPDATE OF reacted ON async_jobs WHEN NEW.reacted=1 AND NEW.reacted_failed=0 BEGIN SELECT RAISE(ABORT, 'fx: legacy reaction blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_legacy_reaction`) })
}

// failFixtureSettle blocks EVERY reaction write against the currently selected
// authority, including the settle-by-failure write (reacted=1 AND
// reacted_failed=1) -- byte-parity with the OFF-mode inline
// `WHEN NEW.reacted=1` injectors. failFixtureReaction is the injector that
// blocks ONLY the ordinary write.
func failFixtureSettle(t *testing.T, conn *sql.DB) {
	t.Helper()
	if batchFlagEnabled() {
		failBatchNodeReaction(t, conn, true)
		failBatchRetainedNoticeReaction(t, conn)
		return
	}
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_legacy_settle BEFORE UPDATE OF reacted ON async_jobs WHEN NEW.reacted=1 BEGIN SELECT RAISE(ABORT, 'fx: legacy settle blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_legacy_settle`) })
}

// failFixturePull fails a delivery-history insert in either runtime mode.
func failFixturePull(t *testing.T, conn *sql.DB) {
	t.Helper()
	if batchFlagEnabled() {
		failBatchPull(t, conn)
		return
	}
	_, err := conn.ExecContext(context.Background(), `CREATE TRIGGER fx_legacy_pull BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'fx: legacy pull blocked'); END`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fx_legacy_pull`) })
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
