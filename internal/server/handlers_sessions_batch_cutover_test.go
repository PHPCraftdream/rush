package server

// R12-F item 6: with RUSH_BATCH_RUNTIME ON the session list's descendant
// flags and the flat panel's awaiting-answer badges are computed ONLY from
// batch facts -- annotateSessionActivity (handlers_sessions.go) maps
// App.SessionActivityBatch's flag-ON facts (app_batch_cutover_facts.go:
// BatchLiveReader over batch_nodes), and the ON-path panel badges only an
// UNREACTED child_question notice (session_notices delivery='pending')
// bound to a live batch root; voided/delivered notices and ordinary
// completion debt never badge.
//
// Every fixture first INSERTs into the REAL schema (the INSERT is the
// first assertion: batch_nodes CHECKs -- roots kind='batch',
// node_id=root_id, tool_call_id/input_hash NOT NULL, task children the
// opposite; session_notices delivery CHECK IN ('none','pending','done',
// 'void')) and then asserts on the reader's answer.
//
// Revert-checks: pointing annotateSessionActivity's flag mapping back at
// the legacy AsyncJobStore readers fails the facts-equality loop below
// (flag ON, the legacy readers see nothing for batch-only fixtures);
// widening the badge to delivered/voided notices fails the void and
// reacted cases of the badge test.

import (
	"context"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/require"
)

// insertBatchCutoverTree seeds one live-or-terminal batch root plus its
// agent task child naming a delegation child session -- the R11 commit
// hook's shape, with the ids parameterized (insertBatchRootAndTask is
// single-use per database).
func insertBatchCutoverTree(t *testing.T, ctx context.Context, a *appPkg.App, rootID, owner, toolCallID, rootState, childID string) {
	t.Helper()
	_, err := a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash, mode, state, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', 0, 'batch', ?, ?, 'parallel', ?, 1700000000, 1700000001)`,
		rootID, rootID, owner, toolCallID, "hash-"+rootID, rootState)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind, task_kind, task_payload, child_session_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', ?, 0, 'task', 'agent', '{}', ?, 'running', 1700000000, 1700000001)`,
		rootID+"-1", rootID, owner, rootID, childID)
	require.NoError(t, err)
}

// insertBatchCutoverDebt seeds one ANNOUNCED, completed root whose task
// child carries the full notice_selected branch (kind, state, sequence>0,
// delivery='done', wake=1, reacted=0) -- the batch outbox debt predicate.
func insertBatchCutoverDebt(t *testing.T, ctx context.Context, a *appPkg.App, rootID, owner string) {
	t.Helper()
	_, err := a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash, mode, state, announced, announce_message_id, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', 0, 'batch', ?, ?, 'parallel', 'completed', 1, ?, 1700000000, 1700000001)`,
		rootID, rootID, owner, "call-"+rootID, "hash-"+rootID, "msg-"+rootID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind, task_kind, state,
		 notice_selected, notice_kind, notice_state, notice_cause, notice_sequence, notice_ordinal, delivery, wake, reacted, created_at, updated_at)
		VALUES (?, ?, ?, 'batch-test-host', ?, 0, 'task', 'agent', 'completed',
		 1, 'task', 'completed', '', 1, 0, 'done', 1, 0, 1700000000, 1700000001)`,
		rootID+"-debt", rootID, owner, rootID)
	require.NoError(t, err)
}

// TestHandleListSessions_BatchRuntimeOnDescendantFlagsFromBatchNodes: the
// flag-ON list annotations are the batch facts, nothing else -- a live
// delegation tree flags HasLiveDescendantWork with the child in
// LiveDescendantIDs, a terminal root and a debt-only session flag nothing,
// and every row's annotation equals App.SessionActivity's flag-ON facts.
func TestHandleListSessions_BatchRuntimeOnDescendantFlagsFromBatchNodes(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	require.True(t, batchRuntimeEnabled())

	live, err := a.Sessions.Create(ctx, "batch live delegator")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "batch-cutover-child", live.ID, "child")
	require.NoError(t, err)
	insertBatchCutoverTree(t, ctx, a, "rb-live", live.ID, "tc-live", "running", child.ID)

	terminal, err := a.Sessions.Create(ctx, "batch terminal delegator")
	require.NoError(t, err)
	terminalChild, err := a.Sessions.CreateTaskSession(ctx, "batch-cutover-terminal-child", terminal.ID, "terminal child")
	require.NoError(t, err)
	insertBatchCutoverTree(t, ctx, a, "rb-done", terminal.ID, "tc-done", "completed", terminalChild.ID)

	debtor, err := a.Sessions.Create(ctx, "batch debt only")
	require.NoError(t, err)
	insertBatchCutoverDebt(t, ctx, a, "rb-debt", debtor.ID)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 512)
	hub.register <- client
	handleListSessions(ctx, a, client, WSMessage{ID: "req", Type: CmdListSessions})
	replies := drainSessionsListReplies(t, client)
	require.NotEmpty(t, replies)
	rows := replies[len(replies)-1]
	require.Len(t, rows, 3, "the three top-level sessions, task children excluded")

	liveRow := findSessionRow(t, rows, live.ID)
	require.True(t, liveRow.HasLiveDescendantWork, "the running batch delegation is live descendant work")
	require.Equal(t, []string{child.ID}, liveRow.LiveDescendantIDs)
	require.False(t, liveRow.HasLiveOwnWork)

	terminalRow := findSessionRow(t, rows, terminal.ID)
	require.False(t, terminalRow.HasLiveDescendantWork, "a terminal batch root is not live work")
	require.Empty(t, terminalRow.LiveDescendantIDs)

	debtorRow := findSessionRow(t, rows, debtor.ID)
	require.False(t, debtorRow.HasLiveDescendantWork, "completion debt is not live work")
	require.False(t, debtorRow.HasLiveOwnWork, "completion debt is not live own work")
	require.Empty(t, debtorRow.LiveDescendantIDs)

	// The annotations are the ONE classifier's flag-ON facts, row by row.
	for _, s := range rows {
		act, err := a.SessionActivity(ctx, s.ID)
		require.NoErrorf(t, err, "single reader of %s", s.ID)
		f := act.Facts
		require.Emptyf(t, f.Unreadable, "no fact read may fail in this fixture (%s)", s.ID)
		require.Equalf(t, f.LiveDelegations > 0, s.HasLiveDescendantWork, "descendant flag vs classifier facts of %s", s.ID)
		require.Equalf(t, f.LiveDescendantSessionIDs, s.LiveDescendantIDs, "descendant ids vs classifier facts of %s", s.ID)
		require.Equalf(t, f.Driver != nil || f.OwnRunningJobs > 0, s.HasLiveOwnWork, "own flag vs classifier facts of %s", s.ID)
		require.Truef(t, f.DriverOwes == (s.ID == debtor.ID), "debt fact of %s", s.ID)
	}
}

// TestBuildSessionLiveWork_BatchAwaitingBadgeRequiresUnreactedPendingNotice
// pins the badge invariant on the flag-ON path: AwaitingAnswer is TRUE only
// for an UNREACTED child_question notice (session_notices delivery='pending')
// bound to a live batch root. A VOIDED notice and a delivered+reacted notice
// must NOT badge; a completion-debt session with no live root has no items
// at all to badge.
func TestBuildSessionLiveWork_BatchAwaitingBadgeRequiresUnreactedPendingNotice(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	newBadgeSession := func(t *testing.T, name string) (*appPkg.App, context.Context, string, string) {
		t.Helper()
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		ctx := t.Context()
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		child, err := a.Sessions.CreateTaskSession(ctx, name+"-child", sess.ID, "child")
		require.NoError(t, err)
		insertBatchCutoverTree(t, ctx, a, "rb-"+name, sess.ID, "tc-"+name, "running", child.ID)
		return a, ctx, sess.ID, "tc-" + name
	}
	insertNotice := func(t *testing.T, ctx context.Context, a *appPkg.App, owner, toolCall, delivery string, reacted int64) {
		t.Helper()
		_, err := a.DB().ExecContext(ctx, `INSERT INTO session_notices
			(owner, kind, text, wake, delivery, reacted, job_tool_call_id, created_at, updated_at)
			VALUES (?, 'child_question', 'Supervision check-in. SUB-AGENT QUESTION (session c): Should I proceed?

preamble.', 0, ?, ?, ?, 1700000002, 1700000002)`,
			owner, delivery, reacted, toolCall)
		require.NoError(t, err)
	}

	t.Run("voided notice does not badge", func(t *testing.T) {
		a, ctx, id, toolCall := newBadgeSession(t, "badge-void")
		insertNotice(t, ctx, a, id, toolCall, "void", 0)
		snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), id)
		require.NoError(t, err)
		require.Len(t, snap.Agents, 1, "the live root is still shown")
		require.False(t, snap.Agents[0].AwaitingAnswer, "a voided child_question notice must not badge")
		require.Empty(t, snap.Agents[0].AwaitingQuestion)
	})

	t.Run("delivered and reacted notice does not badge", func(t *testing.T) {
		a, ctx, id, toolCall := newBadgeSession(t, "badge-reacted")
		insertNotice(t, ctx, a, id, toolCall, "done", 1)
		snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), id)
		require.NoError(t, err)
		require.Len(t, snap.Agents, 1)
		require.False(t, snap.Agents[0].AwaitingAnswer, "a delivered+reacted notice must not badge")
		require.Empty(t, snap.Agents[0].AwaitingQuestion)
	})

	t.Run("completion debt without live work has nothing to badge", func(t *testing.T) {
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		ctx := t.Context()
		sess, err := a.Sessions.Create(ctx, "badge-debt-only")
		require.NoError(t, err)
		insertBatchCutoverDebt(t, ctx, a, "rb-badge-debt", sess.ID)
		snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), sess.ID)
		require.NoError(t, err)
		require.Empty(t, snap.Agents, "completion debt is not live work and never badges")
		require.Empty(t, snap.Commands)
	})
}
