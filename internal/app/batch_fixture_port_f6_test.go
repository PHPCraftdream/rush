// F6 stage-2 test-only batch fixture helpers (app CLI-loop port). The
// agent-package helpers (batch_fixture_port_test.go) are _test files of
// package agent and cannot be imported here, so these f6-prefixed helpers
// reimplement the same dual-mode shape over the app harness's real App:
// RUSH_BATCH_RUNTIME ON seeds batch_nodes through the production BatchStore
// API over application.DB(), OFF keeps the legacy AsyncJobStore claims and
// transitions. No helper mutates the flag: CI selects the mode.
package app

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// f6BatchRunner is the runner identity stamped on every f6 batch fixture
// reservation and projection; the host is always the App's own registered
// host id so the production live reader's host probe answers alive.
const f6BatchRunner = "f6-fixture-runner"

func f6BatchOn() bool { return config.BatchRuntimeEnabled() }

func f6HostID(t *testing.T, application *App) string {
	t.Helper()
	id := application.BatchHostStore().HostID()
	require.NotEmpty(t, id, "the flag-ON app must have a registered host identity")
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
// ids by owner + tool call id.
func f6FindDelegationNodes(t *testing.T, application *App, owner, toolCallID string) (root, node string) {
	t.Helper()
	rows, err := application.DB().QueryContext(context.Background(),
		`SELECT node_id, root_id, kind FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ? ORDER BY rowid`,
		owner, toolCallID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, rootID, kind string
		require.NoError(t, rows.Scan(&id, &rootID, &kind))
		if kind == "batch" {
			root = id
		} else {
			node = id
		}
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, root, "fixture delegation root row missing")
	require.NotEmpty(t, node, "fixture delegation child row missing")
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

// f6ClaimDelegation is the dual-mode running-delegation seed: ON reserves and
// announces a live batch delegation; OFF claims and announces the legacy
// async_jobs row (exactly the pre-port shape).
func f6ClaimDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, childID string) {
	t.Helper()
	if f6BatchOn() {
		f6ReserveDelegation(t, application, owner, toolCallID, childID, "agent", "running")
		return
	}
	_, err := application.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: owner, ToolCallID: toolCallID, Kind: session.JobKindAgent, Input: "x",
		ChildSessionID: childID, ToolName: "agent",
	})
	require.NoError(t, err)
	require.NoError(t, application.asyncJobStore.MarkAnnounced(ctx, owner, toolCallID))
}

// f6CompleteDelegation is the dual-mode completion: ON projects the batch
// nodes terminal (wake selects the completion notice as debt); OFF transitions
// the legacy row (the pre-port write, unchanged).
func f6CompleteDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, summary string, wake bool) {
	t.Helper()
	if f6BatchOn() {
		root, node := f6FindDelegationNodes(t, application, owner, toolCallID)
		f6CompleteDelegationTerminal(t, application, owner, root, node, "completed", summary, wake)
		return
	}
	_, err := application.asyncJobStore.Transition(ctx, session.TransitionParams{
		Owner: owner, ToolCallID: toolCallID, State: "completed", ResultSummary: summary, Wake: wake,
	})
	require.NoError(t, err)
}

// f6ClaimOwnJob is the dual-mode own-background-job seed (an own bash/command
// job with no child session and no announcement, the shape A15 seeds).
func f6ClaimOwnJob(t *testing.T, ctx context.Context, application *App, owner, toolCallID string) {
	t.Helper()
	if f6BatchOn() {
		f6ReserveDelegation(t, application, owner, toolCallID, "", "command", "running")
		return
	}
	_, err := application.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: owner, ToolCallID: toolCallID, Kind: session.JobKindCommand, Input: "heavy", ToolName: "bash",
	})
	require.NoError(t, err)
}

// f6DelegationRunning reports whether toolCallID's delegation is still
// running through the selected authority.
func f6DelegationRunning(t *testing.T, application *App, owner, toolCallID string) bool {
	t.Helper()
	if !f6BatchOn() {
		rows, err := application.asyncJobStore.ListRunningForOwners(context.Background(), []string{owner})
		require.NoError(t, err)
		for _, row := range rows {
			if row.ToolCallID == toolCallID {
				return true
			}
		}
		return false
	}
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
