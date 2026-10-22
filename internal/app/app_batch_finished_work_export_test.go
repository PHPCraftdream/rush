// Acceptance tests for App.batchFinishedWork, the quota
// finished-work export over batch nodes: a one-task root exports ONE row
// keyed by the task with the root's ORIGINAL tool-call id; a multi-task
// root exports each task with ToolCallID = node id; structural batch
// children never export; child-session links are traversed. These tests
// are batch-only by construction: batchFinishedWork IS the
// finished-work reader over the batch authority.
package app

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// the one-task root exports a single row keyed by the task with
// the root's original tool-call id (not the node id) and the root's
// outcome summary as the result.
func TestBatchFinishedWork_OneTaskRootExportsCallID(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	owner := "batch-fw-one"
	_, err := h.app.Sessions.CreateWithID(ctx, owner, owner)
	require.NoError(t, err)
	root := testBatchRoot(owner, "root-fw-one", "call-fw-one", "completed", 1)
	root.outcomeSummary = "one done"
	insertBatchNode(t, h.app, root)
	task := testBatchTask(owner, "root-fw-one", "root-fw-one-t1", "completed", sql.NullString{})
	task.outcomeSummary = "one done"
	insertBatchNode(t, h.app, task)

	rows, incomplete, err := h.app.batchFinishedWork(context.Background(), owner)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, rows, 1, "the one-task root exports exactly one task row, not the root row")
	require.Equal(t, "root-fw-one/root-fw-one-t1", rows[0].Key)
	require.Equal(t, "call-fw-one", rows[0].ToolCallID, "the one-task root keeps its ORIGINAL call id")
	require.Equal(t, "completed", rows[0].State)
	require.True(t, rows[0].Announced)
	require.Equal(t, "one done", rows[0].Result)
	require.Equal(t, "", rows[0].ChildSessionID)
}

// a tree with a nested structural batch child exports ONLY the
// task rows -- the root and the structural child never become rows, the
// nested child's tasks do, in BatchLevelNodes order (ordinal, node_id).
func TestBatchFinishedWork_MultiTaskTreeExportsTasksOnly(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	owner := "batch-fw-multi"
	_, err := h.app.Sessions.CreateWithID(ctx, owner, owner)
	require.NoError(t, err)
	insertBatchNode(t, h.app, testBatchRoot(owner, "root-fw-multi", "call-fw-multi", "completed", 1))
	// Nested STRUCTURAL batch child: mirrors the schema CHECK for non-root
	// batch nodes (tool_call_id/input_hash/announce_message_id NULL).
	b1 := testBatchRoot(owner, "root-fw-multi-b1", "", "completed", 0)
	b1.rootID = "root-fw-multi"
	b1.parent = nullString("root-fw-multi")
	b1.ordinal = 1
	b1.toolCallID = sql.NullString{}
	b1.inputHash = sql.NullString{}
	b1.announceMessageID = sql.NullString{}
	insertBatchNode(t, h.app, b1)
	t1 := testBatchTask(owner, "root-fw-multi", "root-fw-multi-t1", "completed", sql.NullString{})
	t1.parent = nullString("root-fw-multi")
	t1.ordinal = 2
	insertBatchNode(t, h.app, t1)
	t2 := testBatchTask(owner, "root-fw-multi", "root-fw-multi-t2", "completed", sql.NullString{})
	t2.parent = nullString("root-fw-multi-b1")
	t2.ordinal = 3
	insertBatchNode(t, h.app, t2)
	t3 := testBatchTask(owner, "root-fw-multi", "root-fw-multi-t3", "completed", sql.NullString{})
	t3.parent = nullString("root-fw-multi-b1")
	t3.ordinal = 4
	insertBatchNode(t, h.app, t3)

	rows, incomplete, err := h.app.batchFinishedWork(context.Background(), owner)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, rows, 3, "exactly the three task rows export; no root or structural row")
	keys := map[string]bool{}
	for _, row := range rows {
		keys[row.Key] = true
		require.Equal(t, row.Key[len("root-fw-multi/"):], row.ToolCallID, "multi-task root: every ToolCallID is its node id")
	}
	require.Equal(t, map[string]bool{
		"root-fw-multi/root-fw-multi-t1": true,
		"root-fw-multi/root-fw-multi-t2": true,
		"root-fw-multi/root-fw-multi-t3": true,
	}, keys)
	require.NotContains(t, keys, "root-fw-multi/root-fw-multi")
	for key := range keys {
		require.NotContains(t, key, "-b1", "structural batch children never export")
	}
	require.Equal(t, "root-fw-multi/root-fw-multi-t1", rows[0].Key, "stable order: t1 first")
	require.Equal(t, "root-fw-multi/root-fw-multi-t2", rows[1].Key, "stable order: t2 second")
	require.Equal(t, "root-fw-multi/root-fw-multi-t3", rows[2].Key, "stable order: t3 third")
}

// two batch roots sharing one tool-call id export DISTINCT rows
// keyed by root/node identity, so the quota terminal latch on the old
// incarnation never suppresses the new one.
func TestBatchFinishedWork_ReusedCallIDIncarnationsDistinct(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	owner := "batch-fw-inc"
	_, err := h.app.Sessions.CreateWithID(ctx, owner, owner)
	require.NoError(t, err)
	insertBatchNode(t, h.app, testBatchRoot(owner, "root-fw-old", "call-fw-reused", "completed", 1))
	insertBatchNode(t, h.app, testBatchTask(owner, "root-fw-old", "root-fw-old-t1", "completed", sql.NullString{}))
	insertBatchNode(t, h.app, testBatchRoot(owner, "root-fw-new", "call-fw-reused", "running", 1))
	insertBatchNode(t, h.app, testBatchTask(owner, "root-fw-new", "root-fw-new-t1", "completed", sql.NullString{}))

	rows, incomplete, err := h.app.batchFinishedWork(context.Background(), owner)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, rows, 2, "both incarnations export; the shared call id merges nothing")
	byKey := map[string]quotaWorkRow{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	old, okOld := byKey["root-fw-old/root-fw-old-t1"]
	new, okNew := byKey["root-fw-new/root-fw-new-t1"]
	require.True(t, okOld, "the old incarnation exports under its own key")
	require.True(t, okNew, "the new incarnation exports under its own key")
	require.Equal(t, "call-fw-reused", old.ToolCallID)
	require.Equal(t, "call-fw-reused", new.ToolCallID, "both incarnations carry the same reused call id")
	require.Equal(t, "completed", old.State)
	require.Equal(t, "completed", new.State)

	// Pre-latch ONLY the old incarnation, as a previous pass would have;
	// the export predicate (quotaTerminalState && Announced &&
	// !terminal[Key]) still yields the NEW incarnation's key.
	l := &cliLoop{app: h.app, ctx: ctx, sessionID: owner}
	l.quota = &cliQuotaStop{cause: nil, terminal: map[string]bool{"root-fw-old/root-fw-old-t1": true}}
	export := func() []string {
		var finished []string
		for _, row := range rows {
			if l.quotaTerminalState(row.State) && row.Announced && !l.quota.terminal[row.Key] {
				finished = append(finished, row.Key)
			}
		}
		return finished
	}
	require.Equal(t, []string{"root-fw-new/root-fw-new-t1"}, export(),
		"the latch on the old incarnation must not suppress the new one")
}

// the child_session_id delegation link is traversed -- the
// descendant session's one-task root exports its own row keeping its own
// call id, next to the parent's task row carrying the link.
func TestBatchFinishedWork_ChildLinkTraversalExportsDescendantTasks(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	parent, child := "batch-fw-parent", "batch-fw-child"
	for _, id := range []string{parent, child} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	task := testBatchTask(parent, "root-fw-p", "root-fw-p-t1", "completed", sql.NullString{})
	task.childSessionID = nullString(child)
	insertBatchNode(t, h.app, testBatchRoot(parent, "root-fw-p", "call-fw-p", "completed", 1))
	insertBatchNode(t, h.app, task)
	insertBatchNode(t, h.app, testBatchRoot(child, "root-fw-c", "call-fw-c", "completed", 1))
	insertBatchNode(t, h.app, testBatchTask(child, "root-fw-c", "root-fw-c-t1", "completed", sql.NullString{}))

	rows, incomplete, err := h.app.batchFinishedWork(context.Background(), parent)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, rows, 2, "the descendant task exports through the child link")
	byKey := map[string]quotaWorkRow{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	parentRow, okP := byKey["root-fw-p/root-fw-p-t1"]
	childRow, okC := byKey["root-fw-c/root-fw-c-t1"]
	require.True(t, okP, "the parent's task exports")
	require.True(t, okC, "the descendant task is reached through the child link")
	require.Equal(t, child, parentRow.ChildSessionID)
	require.Equal(t, "call-fw-c", childRow.ToolCallID, "the child's one-task root keeps its call id")
}
