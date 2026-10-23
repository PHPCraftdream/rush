// Package R12-F acceptance tests: the app layer reads activity/debt and
// quota finished-work facts ONLY from batch node state.
//
// Fixtures insert real batch_nodes rows through real SQLite so every
// schema CHECK fires (roots: kind='batch', node_id=root_id, parent NULL,
// tool_call_id/input_hash NOT NULL; task children the exact opposite;
// announced=1 requires announce_message_id; debt columns must satisfy the
// notice_selected branch). Every test first asserts on an INSERT/SELECT
// against that real schema (trace table in the R12-F report), then on the
// reader's answer.
package app

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchNodeFixture carries exactly the columns the fixtures set; everything
// else keeps its migration default.
type batchNodeFixture struct {
	nodeID, rootID, owner, host string
	parent                      sql.NullString
	ordinal                     int64
	kind                        string
	toolCallID, inputHash       sql.NullString
	announced                   int64
	announceMessageID           sql.NullString
	mode, taskKind              sql.NullString
	childSessionID              sql.NullString
	state                       string
	noticeSelected              int64
	noticeKind, noticeState     sql.NullString
	noticeCause                 sql.NullString
	noticeSequence, noticeOrd   sql.NullInt64
	delivery, outcomeSummary    string
	wake, reacted               int64
}

// insertBatchNode is the first schema assertion of every test using it: the
// INSERT fails on any violated batch_nodes CHECK/FK.
func insertBatchNode(t *testing.T, application *App, f batchNodeFixture) {
	t.Helper()
	// delivery defaults to the schema default 'none': every unselected fixture
	// row must carry a CHECK-legal value (the delivery CHECK rejects '').
	if f.delivery == "" {
		f.delivery = "none"
	}
	_, err := application.DB().ExecContext(context.Background(), `
INSERT INTO batch_nodes (
	node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
	tool_call_id, input_hash, announced, announce_message_id, mode, task_kind,
	child_session_id, state, notice_selected, notice_kind, notice_state,
	notice_cause, notice_sequence, notice_ordinal, delivery, wake, reacted,
	outcome_summary, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
		f.nodeID, f.rootID, f.owner, f.host, f.parent, f.ordinal, f.kind,
		f.toolCallID, f.inputHash, f.announced, f.announceMessageID, f.mode, f.taskKind,
		f.childSessionID, f.state, f.noticeSelected, f.noticeKind, f.noticeState,
		f.noticeCause, f.noticeSequence, f.noticeOrd, f.delivery, f.wake, f.reacted,
		f.outcomeSummary)
	require.NoError(t, err)
}

func nullString(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
func testBatchRoot(owner, id, toolCall, state string, announced int64) batchNodeFixture {
	return batchNodeFixture{
		nodeID: id, rootID: id, owner: owner, host: "fixture-host", kind: "batch",
		toolCallID: nullString(toolCall), inputHash: nullString("hash-" + id),
		announced: announced, announceMessageID: func() sql.NullString {
			if announced == 1 {
				return nullString("msg-" + id)
			}
			return sql.NullString{}
		}(),
		mode: nullString("parallel"), state: state,
	}
}

func testBatchTask(owner, rootID, id, state string, child sql.NullString) batchNodeFixture {
	return batchNodeFixture{
		nodeID: id, rootID: rootID, owner: owner, host: "fixture-host",
		parent: nullString(rootID), ordinal: 1, kind: "task", taskKind: nullString("shell"),
		childSessionID: child, state: state,
	}
}

// The batch debt node: notice_selected branch fully populated (kind, state,
// cause, sequence > 0, ordinal >= 0, delivery != 'none', wake = 1,
// reacted = 0) under an ANNOUNCED root -- BatchOutboxDebtNodes' predicates.
func testBatchDebtTask(owner, rootID string) batchNodeFixture {
	f := testBatchTask(owner, rootID, rootID+"-debt", "completed", sql.NullString{})
	f.noticeSelected = 1
	f.noticeKind = nullString("task")
	f.noticeState = nullString("completed")
	f.noticeCause = nullString("")
	f.noticeSequence = sql.NullInt64{Int64: 1, Valid: true}
	f.noticeOrd = sql.NullInt64{Int64: 0, Valid: true}
	f.delivery = "done"
	f.wake, f.reacted = 1, 0
	return f
}

func requireBatchNodesEmpty(t *testing.T, application *App) {
	t.Helper()
	var count int
	require.NoError(t, application.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM batch_nodes`).Scan(&count))
	require.Zero(t, count, "fixture premise: no batch_nodes rows exist")
}

// The batch readers answer from batch_nodes only -- a live root reads as
// own running work and a selected unreacted notice reads as debt. Also runs
// with no batch runtime installed beyond the App's own install (the readers
// must not depend on a live Runner): the hand-seeded rows answer directly.
func TestSessionActivityBatch_ReadsBatchNodesOnly(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	live, debtor, clean := "batch-act-live", "batch-act-debt", "batch-act-clean"
	for _, id := range []string{live, debtor, clean} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	// Fixture premise first: no batch_nodes rows anywhere before the seed.
	requireBatchNodesEmpty(t, h.app)
	// live: a RUNNING root plus an already-completed task child.
	insertBatchNode(t, h.app, testBatchRoot(live, "root-live", "call-live", "running", 0))
	insertBatchNode(t, h.app, testBatchTask(live, "root-live", "root-live-task", "completed", sql.NullString{}))
	// debtor: an ANNOUNCED, COMPLETED root whose task child owes a reaction.
	insertBatchNode(t, h.app, testBatchRoot(debtor, "root-debt", "call-debt", "completed", 1))
	insertBatchNode(t, h.app, testBatchDebtTask(debtor, "root-debt"))
	batch, err := h.app.SessionActivityBatch(ctx, []string{live, debtor, clean})
	require.NoError(t, err)
	require.Len(t, batch.ByID, 3)

	liveFacts := batch.ByID[live].Facts
	require.Equal(t, 1, liveFacts.OwnRunningJobs, "the live batch root is own running work")
	require.Zero(t, liveFacts.LiveDelegations)
	require.Empty(t, liveFacts.Unreadable, "no fact read may fail in this fixture")
	require.False(t, batch.ByID[live].Verdict.Kind == session.ActivityEnded,
		"live batch work keeps the session open")

	require.True(t, batch.ByID[debtor].Facts.DriverOwes, "batch notice debt is the debt fact")
	require.Zero(t, batch.ByID[debtor].Facts.OwnRunningJobs, "the debt root is terminal, not live work")
	require.Empty(t, batch.ByID[debtor].Facts.Unreadable)

	cleanFacts := batch.ByID[clean].Facts
	require.False(t, cleanFacts.DriverOwes)
	require.Zero(t, cleanFacts.OwnRunningJobs)
	require.Empty(t, cleanFacts.Unreadable)
	require.Equal(t, session.ActivityIdle, batch.ByID[clean].Verdict.Kind)

	// Single reader == batch reader with one id (the R-ACT invariant).
	single, err := h.app.SessionActivity(ctx, live)
	require.NoError(t, err)
	require.Equal(t, batch.ByID[live], single)
}

// DELETED with H4: TestSessionActivityBatch_BatchRuntimeOffUnchanged -- the
// flag-OFF legacy-row reader is gone (batch_nodes is the only fact source);
// the batch reader contract is pinned by
// TestSessionActivityBatch_ReadsBatchNodesOnly.

// The quota finished-work export enumerates only TASK batch nodes
// across the owner tree -- roots and structural batch children are structure
// and never export; a one-task root keeps the ROOT's original tool-call id
// as the exported identity (Key = RootID/NodeID). The live root's task is
// still enumerated and filtered by the terminal predicate, and the quota
// terminal snapshot dedupes on the root/node Key so a settled node is
// exported at most once.
func TestQuotaFinishedWork_BatchNodes(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	parent, child := "batch-quota-parent", "batch-quota-child"
	for _, id := range []string{parent, child} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	// root-one: terminal AND announced, delegating to the child session.
	rootOne := testBatchRoot(parent, "root-one", "call-one", "completed", 1)
	rootOne.childSessionID = nullString(child)
	rootOne.outcomeSummary = "root-one done"
	insertBatchNode(t, h.app, rootOne)
	t1 := testBatchTask(parent, "root-one", "root-one-t1", "completed", sql.NullString{})
	t1.outcomeSummary = "root-one done"
	insertBatchNode(t, h.app, t1)
	// root-two: still live, with a running task child.
	insertBatchNode(t, h.app, testBatchRoot(parent, "root-two", "call-two", "running", 0))
	insertBatchNode(t, h.app, testBatchTask(parent, "root-two", "root-two-t1", "running", sql.NullString{}))
	// child-owned terminal root, reached only through the delegation link.
	insertBatchNode(t, h.app, testBatchRoot(child, "root-child", "call-child", "completed", 1))
	insertBatchNode(t, h.app, testBatchTask(child, "root-child", "root-child-t1", "completed", sql.NullString{}))

	l := &cliLoop{app: h.app, ctx: ctx, sessionID: parent}
	rows, incomplete, err := l.quotaJobsInTree(ctx)
	require.NoError(t, err)
	require.False(t, incomplete)
	byKey := map[string]quotaWorkRow{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	// The export enumerates TASK nodes only -- roots and structural batch
	// nodes are structure and never export. A one-task root keeps the root's
	// original tool-call id as the exported identity, and the walk still
	// enumerates the live root's task, filtered by the terminal predicate.
	require.Len(t, byKey, 3, "exactly the three task rows export; roots are structure")
	require.Equal(t, "call-one", byKey["root-one/root-one-t1"].ToolCallID, "the one-task root keeps its original call id")
	require.Equal(t, "completed", byKey["root-one/root-one-t1"].State)
	require.True(t, byKey["root-one/root-one-t1"].Announced, "the announced task is exported")
	require.Equal(t, "root-one done", byKey["root-one/root-one-t1"].Result)
	require.Equal(t, "call-child", byKey["root-child/root-child-t1"].ToolCallID, "the delegation child's one-task root keeps its call id")
	require.Equal(t, "running", byKey["root-two/root-two-t1"].State, "the live root's task is enumerated, filtered by the terminal predicate instead")
	require.NotContains(t, byKey, "root-one/root-one", "roots never export")
	require.NotContains(t, byKey, "root-two/root-two", "roots never export")
	require.NotContains(t, byKey, "root-child/root-child", "roots never export")

	// The live root is not finished work; pending/starting are not either.
	require.True(t, l.quotaTerminalState("completed"))
	require.False(t, l.quotaTerminalState("running"))
	require.False(t, l.quotaTerminalState("pending"), "non-terminal batch states are not finished")

	// Full export predicate (quotaPhase's), before the terminal snapshot:
	// exactly the announced terminal task rows, the running task excluded.
	export := func() []string {
		var finished []string
		for _, row := range rows {
			if l.quotaTerminalState(row.State) && row.Announced && !l.quota.terminal[row.Key] {
				finished = append(finished, row.Key)
			}
		}
		return finished
	}
	l.quota = &cliQuotaStop{cause: nil, terminal: map[string]bool{}}
	require.ElementsMatch(t, []string{"root-one/root-one-t1", "root-child/root-child-t1"}, export())

	// The terminal snapshot marks every already-terminal node, so a later
	l.snapshotQuotaTerminal()
	l.quota = &cliQuotaStop{cause: nil, terminal: map[string]bool{}}
	require.ElementsMatch(t, []string{"root-one/root-one-t1", "root-child/root-child-t1"}, export())

	// The terminal snapshot marks every already-terminal node, so a later
	// pass exports nothing twice (the legacy latch semantics).
	l.snapshotQuotaTerminal()
	require.False(t, l.quota.snapshotIncomplete)
	require.Empty(t, export())
}

// DELETED with H4: TestQuotaFinishedWork_BatchRuntimeOffUnchanged -- the
// flag-OFF legacy-row export (Claim/Transition seeded, "any non-running
// state" predicate) is gone with the authority; the batch export contract
// is pinned by TestQuotaFinishedWork_BatchNodes above and
// app_batch_finished_work_export_test.go.
