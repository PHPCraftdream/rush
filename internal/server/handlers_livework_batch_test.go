package server

// R12-C: the flat live-work snapshot is built ONLY from batch_nodes (live
// roots) — no async_jobs table exists in the final schema and none is
// queried on that path; an incomplete read keeps
// the previously shown work (complete=false, and the pusher does not
// broadcast a partial snapshot).
//
// Revert-check: pointing the batch branch back at AsyncJobStore.JobsInTree
// fails TestBuildSessionLiveWork_BatchOnlySchemaHasNoAsyncJobsTable (the
// fresh store has no async_jobs table at all, so the old-table read errors
// and no snapshot arrives); an
// incomplete read that still replaces (broadcasts) fails
// TestLiveWorkPusher_SkipsIncompleteOnPathSnapshot; dropping the
// complete=false downgrade fails
// TestBuildSessionLiveWork_IncompleteBatchReadKeepsWork.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// insertBatchRootAndTask seeds one live batch root ('rb', tool call tc-b)
// with a single agent task child in the real test database — the same shape
// the R11 commit hook writes (root rows are kind='batch' with tool_call_id
// and input_hash; task children carry task_kind/task_payload).
func insertBatchRootAndTask(t *testing.T, ctx context.Context, a *appPkg.App, owner, childSession string) {
	t.Helper()
	_, err := a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, ordinal, kind, tool_call_id, input_hash, mode, state, created_at, updated_at)
		VALUES ('rb', 'rb', ?, 'batch-test-host', 0, 'batch', 'tc-b', 'hash-b', 'parallel', 'running', 1700000000, 1700000001)`, owner)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx, `INSERT INTO batch_nodes
		(node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind, task_kind, task_payload, child_session_id, state, created_at, updated_at)
		VALUES ('rb-1', 'rb', ?, 'batch-test-host', 'rb', 0, 'task', 'agent', '{}', ?, 'running', 1700000000, 1700000001)`,
		owner, sql.NullString{String: childSession, Valid: childSession != ""})
	require.NoError(t, err)
}

func TestBuildSessionLiveWork_BatchOnlySchemaHasNoAsyncJobsTable(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "lw batch on")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "lw-batch-child", sess.ID, "child")
	require.NoError(t, err)
	insertBatchRootAndTask(t, ctx, a, sess.ID, child.ID)

	// Prove the final schema genuinely has NO async_jobs table: the fresh
	// store must not contain it (any legacy read of it would error).
	var n int
	require.NoError(t, a.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE name='async_jobs'").Scan(&n))
	require.Zero(t, n, "async_jobs must be absent from the final schema")

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	snap := requestSnapshot(t, a, client, sess.ID)
	require.NotNil(t, snap, "the ON path must answer without the old table")
	require.NotNil(t, snap.Complete, "the ON path always sets the complete pointer")
	require.True(t, *snap.Complete, "a fully-read walk reports complete")
	require.Len(t, snap.Agents, 1, "the batch root's agent task is the delegation item")
	require.Empty(t, snap.Commands)
	agent := snap.Agents[0]
	require.Equal(t, "tc-b", agent.ToolCallID)
	require.Equal(t, "agent", agent.ToolName)
	require.Equal(t, child.ID, agent.ChildSessionID)
	require.Equal(t, "running", agent.Status)
	require.Equal(t, int64(1700000000000), agent.StartedAt)
	require.Zero(t, agent.FinishedAt)
}

func TestBuildSessionLiveWork_IncompleteBatchReadKeepsWork(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "lw incomplete")
	require.NoError(t, err)

	// The live root links a child session whose chunk read FAILS: the walk
	// is incomplete, but the root that WAS read must still be carried (an
	// incomplete read never drops the active work it saw).
	root := batchNode("rb", "rb", sess.ID, "running", "tc-b", 0)
	root.Kind = "batch"
	root.InputHash = sql.NullString{String: "hash-b", Valid: true}
	root.ChildSessionID = sql.NullString{String: "child-gone", Valid: true}
	task := batchNode("rb-1", "rb", sess.ID, "running", "", 0)
	task.ParentID = sql.NullString{String: "rb", Valid: true}
	task.TaskKind = sql.NullString{String: "agent", Valid: true}
	src := &fakeBatchSource{rows: []db.BatchNode{root, task}, failOn: map[string]bool{"child-gone": true}}
	withBatchSeams(t, src, probeAlive, nil)

	snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snap.Complete, "the ON path always sets the complete pointer")
	require.False(t, *snap.Complete, "a failed chunk must mark the snapshot incomplete")
	require.Len(t, snap.Agents, 1, "the read root is kept, not dropped")
	require.Equal(t, "tc-b", snap.Agents[0].ToolCallID)
}

func TestBuildSessionLiveWork_BatchAwaitingBadgeOnPath(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "lw awaiting batch")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "lw-batch-q-child", sess.ID, "child")
	require.NoError(t, err)
	insertBatchRootAndTask(t, ctx, a, sess.ID, child.ID)
	_, err = a.DB().ExecContext(ctx, `INSERT INTO session_notices
		(owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
		VALUES (?, 'child_question', 'Supervision check-in. SUB-AGENT QUESTION (session `+child.ID+`): Should I proceed with the risky step?

More preamble.', 0, 'pending', 'tc-b', 1700000002, 1700000002)`, sess.ID)
	require.NoError(t, err)

	snap, err := buildSessionLiveWork(ctx, a, newTitleCache(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snap.Complete)
	require.True(t, *snap.Complete)
	require.Len(t, snap.Agents, 1)
	require.True(t, snap.Agents[0].AwaitingAnswer, "the pending child_question notice must badge the held root")
	require.Equal(t, "Should I proceed with the risky step?", snap.Agents[0].AwaitingQuestion)
}

func TestLiveWorkPusher_SkipsIncompleteOnPathSnapshot(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, "lw pusher incomplete")
	require.NoError(t, err)

	// Every walk fails: the only readable snapshot is incomplete, so the
	// pusher must NOT broadcast a replacement — the client keeps whatever
	// it already shows.
	src := &fakeBatchSource{failOn: map[string]bool{sess.ID: true}}
	withBatchSeams(t, src, probeAlive, nil)

	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client

	p := startLiveWorkPusher(ctx, a, hub)
	t.Cleanup(func() { <-p.stopped })
	p.mark(sess.ID)
	require.Empty(t, collectLiveWorkSnapshots(t, client, 700*time.Millisecond),
		"an incomplete ON-path read must never replace the client's snapshot")
}
