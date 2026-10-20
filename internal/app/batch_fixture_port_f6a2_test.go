// F6a-2 stage-2 test-only helpers. They exist because the shared F6 finder
// (f6FindDelegationNodes, batch_fixture_port_f6_test.go) matches children by
// tool_call_id, but batchInsertTree stamps tool_call_id on the ROOT row only
// (internal/session/batch_store.go:135-138 clears it for every child), so the
// child lookup can never succeed under RUSH_BATCH_RUNTIME=1. These helpers
// resolve the child from the root's snapshot instead, and add the dual-mode
// own-command-job seed/completion/readback the billing port needs. They never
// mutate the flag and never touch the shared F6 file.
package app

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// f6a2FindDelegationNodes locates one fixture delegation's root and child node
// ids by owner + tool call id. ON mode resolves the root row (the only row
// carrying tool_call_id) and takes the child from the snapshot Read; OFF mode
// never runs (callers branch on f6BatchOn first).
func f6a2FindDelegationNodes(t *testing.T, application *App, owner, toolCallID string) (root, node string) {
	t.Helper()
	var rootID string
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT node_id FROM batch_nodes
		 WHERE owner_session_id = ? AND tool_call_id = ? AND node_id = root_id
		 ORDER BY rowid DESC LIMIT 1`,
		owner, toolCallID).Scan(&rootID),
		"fixture delegation root row missing for tool call %s", toolCallID)
	snap, err := session.NewBatchStore(application.DB()).Read(context.Background(), owner, rootID)
	require.NoError(t, err)
	for _, n := range snap.Nodes {
		if n.NodeID != rootID {
			return rootID, n.NodeID
		}
	}
	t.Fatalf("fixture delegation root %s has no child node", rootID)
	return "", ""
}

// f6a2CompleteDelegation is the dual-mode completion used by the F6a-2 ported
// tests: ON resolves the nodes via f6a2FindDelegationNodes and projects both
// terminal through the shared f6CompleteDelegationTerminal (wake selects the
// child completion notice as reaction debt); OFF transitions the legacy row
// unchanged. Fatal on failure: use f6a2CompleteDelegationErr from background
// goroutines that must report through a channel instead of runtime.Goexit.
func f6a2CompleteDelegation(t *testing.T, ctx context.Context, application *App, owner, toolCallID, summary string, wake bool) {
	t.Helper()
	require.NoError(t, f6a2CompleteDelegationErr(ctx, application, owner, toolCallID, summary, wake))
}

// f6a2CompleteDelegationErr is the error-returning completion: every failure
// path returns instead of failing the test, so a caller goroutine can always
// deliver the outcome on its channel.
func f6a2CompleteDelegationErr(ctx context.Context, application *App, owner, toolCallID, summary string, wake bool) error {
	if !f6BatchOn() {
		_, err := application.asyncJobStore.Transition(ctx, session.TransitionParams{
			Owner: owner, ToolCallID: toolCallID, State: "completed", ResultSummary: summary, Wake: wake,
		})
		return err
	}
	var rootID string
	err := application.DB().QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes
		 WHERE owner_session_id = ? AND tool_call_id = ? AND node_id = root_id
		 ORDER BY rowid DESC LIMIT 1`,
		owner, toolCallID).Scan(&rootID)
	if err != nil {
		return fmt.Errorf("fixture delegation root row missing for tool call %s: %w", toolCallID, err)
	}
	snap, err := session.NewBatchStore(application.DB()).Read(ctx, owner, rootID)
	if err != nil {
		return fmt.Errorf("fixture delegation snapshot read failed: %w", err)
	}
	node := ""
	for _, n := range snap.Nodes {
		if n.NodeID != rootID {
			node = n.NodeID
			break
		}
	}
	if node == "" {
		return fmt.Errorf("fixture delegation root %s has no child node", rootID)
	}
	proj := session.BatchProjection{
		Host: application.BatchHostStore().HostID(), RunnerID: f6BatchRunner, Sequence: 2,
		Roots: []session.BatchRootProjection{{Owner: owner, Root: rootID, Updates: []db.ProjectBatchNodeParams{
			{NodeID: rootID, State: "completed", OutcomeState: "completed", OutcomeSummary: summary, ControlState: "none"},
			{NodeID: node, State: "completed", OutcomeState: "completed", OutcomeSummary: summary, ControlState: "none"},
		}}},
	}
	if wake {
		proj.Roots[0].Notices = []db.SelectBatchNodeNoticeParams{{
			NodeID:     node,
			NoticeKind: sql.NullString{String: "task", Valid: true}, NoticeState: sql.NullString{String: "completed", Valid: true}, NoticeCause: sql.NullString{String: "", Valid: true},
			NoticeOrdinal: sql.NullInt64{Int64: 1, Valid: true}, NoticeCompleted: 1, Wake: 1,
		}}
	}
	return session.NewBatchStore(application.DB()).Project(ctx, proj)
}

// f6a2ClaimOwnCommandJob is the dual-mode own command job seed: ON reserves
// and announces a live "command" batch root (the announced live shape the
// production bash background path leaves); OFF claims and announces the
// legacy async_jobs row, the pre-port shape byte-identical.
func f6a2ClaimOwnCommandJob(t *testing.T, ctx context.Context, application *App, owner, toolCallID string) {
	t.Helper()
	if f6BatchOn() {
		f6ReserveDelegation(t, application, owner, toolCallID, "", "command", "running")
		return
	}
	_, err := application.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: owner, ToolCallID: toolCallID, Kind: session.JobKindCommand,
		ToolName: "bash", Input: "durable test command",
	})
	require.NoError(t, err)
	require.NoError(t, application.asyncJobStore.MarkAnnounced(ctx, owner, toolCallID))
}

// f6a2OwnJobReadback is the dual-mode "exactly one completed own job"
// readback. OFF counts the owner's legacy async_jobs rows (the pre-port
// require.Len). ON counts the owner's completed batch ROOTS (one per logical
// job; the task child is structure, not a second job), so a duplicate job is
// still detected. state/delivery come from the row whose completion is
// actually pending delivery (the selected child), mirroring the legacy row's
// columns. The separate double-export of root and child by batchFinishedWork
// is pinned by the unchanged FinishedWork assertion of the billing test, not
// by this readback.
func f6a2OwnJobReadback(t *testing.T, application *App, owner, toolCallID string) (count int, state, delivery string) {
	t.Helper()
	if f6BatchOn() {
		require.NoError(t, application.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ? AND node_id = root_id AND state = 'completed'`,
			owner).Scan(&count))
		if count > 0 {
			state = "completed"
			require.NoError(t, application.DB().QueryRowContext(context.Background(),
				`SELECT delivery FROM batch_nodes
				 WHERE owner_session_id = ? AND state = 'completed' AND notice_selected = 1
				 ORDER BY rowid LIMIT 1`,
				owner).Scan(&delivery))
		}
		return count, state, delivery
	}
	rows, err := application.asyncJobStore.ListAsyncJobsForOwner(context.Background(), owner)
	require.NoError(t, err)
	count = len(rows)
	if count > 0 {
		state, delivery = rows[0].State, rows[0].Delivery
	}
	return count, state, delivery
}

// f6a2ChildSessionOf returns owner's delegation child session id: ON the
// batch task node's durable child_session_id link, OFF the legacy async_jobs
// row's link. The ON read PINS the production contract that the batch agent
// driver's BindChildSession (batch_exec_agent.go:196-199) must persist the
// link durably: reservation never sets it (batch_commit.go:151-163) and the
// engine projection leaves it NULL (batch_commit.go:371-394), so under the
// current production code this lookup finds nothing — see finding #5 in
// .rush/stdin/jb-f6a2.findings.md. It deliberately does NOT fall back to the
// sessions-table parent link, which would mask the missing persistence.
func f6a2ChildSessionOf(t *testing.T, application *App, owner string) string {
	t.Helper()
	if f6BatchOn() {
		var child string
		require.NoError(t, application.DB().QueryRowContext(context.Background(),
			`SELECT child_session_id FROM batch_nodes
			 WHERE owner_session_id = ? AND child_session_id IS NOT NULL AND child_session_id != ''
			 ORDER BY rowid DESC LIMIT 1`,
			owner).Scan(&child), "no delegation child link for %s (finding #5)", owner)
		return child
	}
	rows, err := application.asyncJobStore.ListAsyncJobsForOwner(context.Background(), owner)
	require.NoError(t, err)
	for _, row := range rows {
		if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
			return row.ChildSessionID.String
		}
	}
	t.Fatal("no delegation row")
	return ""
}
