// F6 stage-2 test-only batch fixture helpers (app CLI-loop port). The
// agent-package helpers (batch_fixture_port_test.go) are _test files of
// package agent and cannot be imported here, so these f6-prefixed helpers
// reimplement the same shape over the app harness's real App: they seed
// batch_nodes ONLY through the production BatchStore API (Reserve /
// BatchAnnounceStarted / Project) over application.DB(), so every schema
// CHECK and FK fires. Batch-only (H4 atomic activation): no environment
// variable is read and no legacy async_jobs row is ever written.
package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// f6BatchRunner is the runner identity stamped on every f6 batch fixture
// reservation and projection; the host is always the App's own registered
// host id so the production live reader's host probe answers alive.
const f6BatchRunner = "f6-fixture-runner"

// f6BatchOn is the H4 final-world stub retained ONLY as a cross-chunk seam:
// other P7 test files (not this lane's chunk) still branch on it while their
// own rewrites land. Batch-only: it always answers true and never reads an
// environment variable. Remove together with the last caller.
func f6BatchOn() bool { return true }

func f6HostID(t *testing.T, application *App) string {
	t.Helper()
	id := application.BatchHostStore().HostID()
	require.NotEmpty(t, id, "the app must have a registered host identity")
	return id
}

// f6ReserveDelegation reserves a batch root with ONE task child (kind
// taskKind, state as given, optional child-session link) and announces the
// started root, mirroring the production agent-tool delegation shape.
func f6ReserveDelegation(t *testing.T, application *App, owner, toolCallID, childID, taskKind, state string) (root, node string) {
	t.Helper()
	ctx := context.Background()
	store := session.NewBatchStore(application.DB())
	res, err := store.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: f6HostID(t, application), RunnerID: f6BatchRunner, ToolCallID: toolCallID,
		InputHash: "f6-hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: taskKind, Valid: true},
			ChildSessionID: sql.NullString{String: childID, Valid: childID != ""},
			State:          state, ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, node = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	start, err := store.BatchAnnounceStarted(ctx, application.Messages, owner, root,
		message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "f6 fixture delegation started"}},
			// A started delegation is a background-job announcement: the same
			// shape the production announce path persists.
			BackgroundJobNotice: true,
		})
	require.NoError(t, err)
	require.NotEmpty(t, start.ID)
	return root, node
}

// f6FindDelegationNodes locates one fixture delegation's root and child node
// ids by owner + tool call id. Only the ROOT row carries tool_call_id
// (batchInsertTree stamps it on the root alone, children are NULL by design),
// so the root is found by tool call id and the task child through root_id.
func f6FindDelegationNodes(t *testing.T, application *App, owner, toolCallID string) (root, node string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ? AND kind = 'batch' ORDER BY rowid DESC LIMIT 1`,
		owner, toolCallID).Scan(&root), "fixture delegation root row missing")
	require.NoError(t, application.DB().QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes WHERE root_id = ? AND kind = 'task' ORDER BY rowid LIMIT 1`,
		root).Scan(&node), "fixture delegation child row missing")
	return root, node
}

// f6CompleteDelegationTerminal projects one fixture delegation root AND its
// child to the terminal state. withNotice selects the child completion notice
// (wake=1, delivery pending) so the completion becomes reaction debt, the
// same fact the legacy Transition's Wake flag produces.
func f6CompleteDelegationTerminal(t *testing.T, application *App, owner, root, node, state, summary string, withNotice bool) {
	t.Helper()
	proj := session.BatchProjection{
		Host: f6HostID(t, application), RunnerID: f6BatchRunner, Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: root, Updates: []db.ProjectBatchNodeParams{
			{NodeID: root, State: state, OutcomeState: state, OutcomeSummary: summary, ControlState: "none"},
			{NodeID: node, State: state, OutcomeState: state, OutcomeSummary: summary, ControlState: "none"},
		}}},
	}
	if withNotice {
		proj.Roots[0].Notices = []db.SelectBatchNodeNoticeParams{{
			NodeID:     node,
			NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: state, Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
			NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
		}}
	}
	require.NoError(t, session.NewBatchStore(application.DB()).Project(context.Background(), proj))
}

// f6ClaimDelegation is the running-delegation seed: it reserves and announces
// a live batch delegation (an "agent" task child, optionally linked to its
// child session). The ctx parameter is retained for caller signature
// stability; the batch seed drives its own context.
func f6ClaimDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, childID string) {
	t.Helper()
	_ = ctx
	f6ReserveDelegation(t, application, owner, toolCallID, childID, "agent", "running")
}

// f6CompleteDelegation is the completion: it projects the batch nodes
// terminal (wake selects the completion notice as reaction debt).
func f6CompleteDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, summary string, wake bool) {
	t.Helper()
	_ = ctx
	root, node := f6FindDelegationNodes(t, application, owner, toolCallID)
	f6CompleteDelegationTerminal(t, application, owner, root, node, "completed", summary, wake)
}

// f6ClaimOwnJob is the own-background-job seed (an own command job with no
// child session, the shape A15 seeds).
func f6ClaimOwnJob(t *testing.T, ctx context.Context, application *App, owner, toolCallID string) {
	t.Helper()
	_ = ctx
	f6ReserveDelegation(t, application, owner, toolCallID, "", "command", "running")
}

// f6DelegationRunning reports whether toolCallID's delegation is still
// running: the live batch task child's state.
func f6DelegationRunning(t *testing.T, application *App, owner, toolCallID string) bool {
	t.Helper()
	_, node := f6FindDelegationNodes(t, application, owner, toolCallID)
	var state string
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT state FROM batch_nodes WHERE node_id = ?`, node).Scan(&state))
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}
