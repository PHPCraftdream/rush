// Package R12-F acceptance tests: with RUSH_BATCH_RUNTIME ON the app layer
// reads activity/debt and quota finished-work facts ONLY from batch node
// state; flag OFF stays byte-for-byte on the AsyncJobStore readers.
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
	delivery                    string
	wake, reacted               int64
}

// insertBatchNode is the first schema assertion of every test using it: the
// INSERT fails on any violated batch_nodes CHECK/FK.
func insertBatchNode(t *testing.T, application *App, f batchNodeFixture) {
	t.Helper()
	_, err := application.DB().ExecContext(context.Background(), `
INSERT INTO batch_nodes (
	node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
	tool_call_id, input_hash, announced, announce_message_id, mode, task_kind,
	child_session_id, state, notice_selected, notice_kind, notice_state,
	notice_cause, notice_sequence, notice_ordinal, delivery, wake, reacted,
	created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%s','now'), strftime('%s','now'))`,
		f.nodeID, f.rootID, f.owner, f.host, f.parent, f.ordinal, f.kind,
		f.toolCallID, f.inputHash, f.announced, f.announceMessageID, f.mode, f.taskKind,
		f.childSessionID, f.state, f.noticeSelected, f.noticeKind, f.noticeState,
		f.noticeCause, f.noticeSequence, f.noticeOrd, f.delivery, f.wake, f.reacted)
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
	require.NoError(t, application.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM async_jobs`).Scan(&count))
	require.Zero(t, count, "fixture premise: no async_jobs rows exist")
}

// Flag ON: activity/debt facts come from batch_nodes only -- a live root
// reads as own running work and a selected unreacted notice reads as debt,
// with ZERO async_jobs rows in the database. Also runs with no batch
// runtime installed (the App is built flag-off, so no provider/runtime is
// configured): the flag-ON readers must not depend on one.
func TestSessionActivityBatch_BatchRuntimeOnReadsBatchNodesOnly(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false") // build without the runtime install
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	live, debtor, clean := "batch-act-live", "batch-act-debt", "batch-act-clean"
	for _, id := range []string{live, debtor, clean} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	// live: a RUNNING root plus an already-completed task child.
	insertBatchNode(t, h.app, testBatchRoot(live, "root-live", "call-live", "running", 0))
	insertBatchNode(t, h.app, testBatchTask(live, "root-live", "root-live-task", "completed", sql.NullString{}))
	// debtor: an ANNOUNCED, COMPLETED root whose task child owes a reaction.
	insertBatchNode(t, h.app, testBatchRoot(debtor, "root-debt", "call-debt", "completed", 1))
	insertBatchNode(t, h.app, testBatchDebtTask(debtor, "root-debt"))
	requireBatchNodesEmpty(t, h.app)

	t.Setenv("RUSH_BATCH_RUNTIME", "true") // read under the flag, no runtime installed
	batch, err := h.app.SessionActivityBatch(ctx, []string{live, debtor, clean})
	require.NoError(t, err)
	require.Len(t, batch.ByID, 3)

	liveFacts := batch.ByID[live].Facts
	require.Equal(t, 1, liveFacts.OwnRunningJobs, "flag ON: the live batch root is own running work")
	require.Zero(t, liveFacts.LiveDelegations)
	require.Empty(t, liveFacts.Unreadable, "no fact read may fail in this fixture")
	require.False(t, batch.ByID[live].Verdict.Kind == session.ActivityEnded,
		"flag ON: live batch work keeps the session open")

	require.True(t, batch.ByID[debtor].Facts.DriverOwes, "flag ON: batch notice debt is the debt fact")
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

// Flag OFF: the reader answers from the legacy rows and IGNORES batch node
// state entirely (a live batch root on the third session changes nothing).
func TestSessionActivityBatch_BatchRuntimeOffUnchanged(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	oldLive, oldDebt, batchOnly := "batch-off-live", "batch-off-debt", "batch-off-batch"
	for _, id := range []string{oldLive, oldDebt, batchOnly} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	_, err := h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: oldLive, ToolCallID: "own-bash", Kind: session.JobKindCommand, ToolName: "bash", Input: "x"})
	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, oldDebt, "wake", "x", true, ""))
	insertBatchNode(t, h.app, testBatchRoot(batchOnly, "root-off", "call-off", "running", 0))

	batch, err := h.app.SessionActivityBatch(ctx, []string{oldLive, oldDebt, batchOnly})
	require.NoError(t, err)
	require.Equal(t, 1, batch.ByID[oldLive].Facts.OwnRunningJobs, "flag OFF: the legacy running row is the live-work fact")
	require.True(t, batch.ByID[oldDebt].Facts.DriverOwes, "flag OFF: the legacy notice is the debt fact")
	require.Zero(t, batch.ByID[batchOnly].Facts.OwnRunningJobs, "flag OFF: batch nodes are ignored")
	require.False(t, batch.ByID[batchOnly].Facts.DriverOwes)
	for _, id := range []string{oldLive, oldDebt, batchOnly} {
		require.Empty(t, batch.ByID[id].Facts.Unreadable)
	}
}

// Flag ON: the quota finished-work export enumerates terminal batch
// roots/nodes across the owner tree (terminal roots INCLUDED via the
// history query), skips the live root as work-in-progress, and the quota
// terminal snapshot dedupes on the root/node identity so a settled node is
// exported at most once.
func TestQuotaFinishedWork_BatchRuntimeOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
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
	insertBatchNode(t, h.app, rootOne)
	insertBatchNode(t, h.app, testBatchTask(parent, "root-one", "root-one-task", "completed", sql.NullString{}))
	// root-two: still live -- never finished work.
	insertBatchNode(t, h.app, testBatchRoot(parent, "root-two", "call-two", "running", 0))
	// child-owned terminal root, reached only through the delegation link.
	insertBatchNode(t, h.app, testBatchRoot(child, "root-child", "call-child", "completed", 1))

	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	l := &cliLoop{app: h.app, ctx: ctx, sessionID: parent}
	rows, incomplete, err := l.quotaJobsInTree(ctx)
	require.NoError(t, err)
	require.False(t, incomplete)
	byKey := map[string]quotaWorkRow{}
	for _, row := range rows {
		byKey[row.Key] = row
	}
	// The tree walk saw every node of every root, terminal or not.
	require.Len(t, byKey, 4)
	require.Equal(t, "call-one", byKey["root-one/root-one"].ToolCallID)
	require.Equal(t, "completed", byKey["root-one/root-one"].State)
	require.True(t, byKey["root-one/root-one"].Announced, "the announced root is exported")
	require.Equal(t, "root-one done", byKey["root-one/root-one"].Result)
	require.Equal(t, "root-one-task", byKey["root-one/root-one-task"].ToolCallID)
	require.Equal(t, "call-child", byKey["root-child/root-child"].ToolCallID, "the delegation child's terminal root is in the tree")
	_, ok := byKey["root-two/root-two"]
	require.True(t, ok, "the live root is enumerated, filtered by the terminal predicate instead")

	// The live root is not finished work; pending/starting are not either.
	require.True(t, l.quotaTerminalState("completed"))
	require.False(t, l.quotaTerminalState("running"))
	require.False(t, l.quotaTerminalState("pending"), "flag ON: non-terminal batch states are not finished")

	// Full export predicate (quotaPhase's), before the terminal snapshot:
	// exactly the announced terminal nodes, the live root excluded.
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
	require.ElementsMatch(t, []string{"root-one/root-one", "root-one/root-one-task", "root-child/root-child"}, export())

	// The terminal snapshot marks every already-terminal node, so a later
	// pass exports nothing twice (the legacy latch semantics).
	l.snapshotQuotaTerminal()
	require.False(t, l.quota.snapshotIncomplete)
	require.Empty(t, export())
}

// Flag OFF: the export still reads the legacy rows (Claim/Transition seeded)
// and never the batch nodes; the terminal predicate stays "any non-running
// state".
func TestQuotaFinishedWork_BatchRuntimeOffUnchanged(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	_, err := h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: h.sessionID, ToolCallID: "own-bash", Kind: session.JobKindCommand, ToolName: "bash", Input: "x"})
	require.NoError(t, err)
	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "own-bash"))
	_, err = h.app.asyncJobStore.Transition(ctx, session.TransitionParams{Owner: h.sessionID, ToolCallID: "own-bash", State: "completed", ResultSummary: "own-output", Wake: true})
	require.NoError(t, err)
	insertBatchNode(t, h.app, testBatchRoot(h.sessionID, "root-off", "call-off", "completed", 1))

	l := &cliLoop{app: h.app, ctx: ctx, sessionID: h.sessionID}
	rows, incomplete, err := l.quotaJobsInTree(ctx)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, rows, 1, "flag OFF: only the legacy row is read")
	require.Equal(t, "own-bash", rows[0].ToolCallID)
	require.Equal(t, "own-output", rows[0].Result)
	require.True(t, rows[0].Announced)
	require.True(t, l.quotaTerminalState("completed"))
	require.True(t, l.quotaTerminalState("pending"), "flag OFF: the legacy predicate is any non-running state")
}
