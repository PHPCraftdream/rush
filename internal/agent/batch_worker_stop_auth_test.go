package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func workerScopedStopContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchWorkerStopScopeKey{}, true)
}

type workerStopTestExecutor struct {
	started chan jobbatch.Launch
}

type workerStopTestExecution struct{ cancel context.CancelFunc }

func (e workerStopTestExecution) Stop(context.Context) error {
	e.cancel()
	return nil
}

func (e *workerStopTestExecutor) Start(ctx context.Context, launch jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	execCtx, cancel := context.WithCancel(ctx)
	e.started <- launch
	go func() {
		<-execCtx.Done()
		report(jobbatch.Result{State: jobbatch.StateCancelled})
	}()
	return workerStopTestExecution{cancel: cancel}, nil
}

func TestInstalledModelControlWorkerStopScope(t *testing.T) {
	executor := &workerStopTestExecutor{started: make(chan jobbatch.Launch, 32)}
	env := newBatchInventoryEnvWithExecutor(t, "view", executor)
	c := batchOwnedControl(t, env, batchControlProfile(t))
	rows := []session.Session{
		{ID: "worker", ParentSessionID: env.sessionID},
		{ID: "child", ParentSessionID: "worker"},
		{ID: "sibling", ParentSessionID: env.sessionID},
		{ID: "stranger"},
	}
	for _, row := range rows {
		_, err := env.q.CreateSession(env.ctx, db.CreateSessionParams{ID: row.ID, Title: row.ID, ParentSessionID: sql.NullString{String: row.ParentSessionID, Valid: row.ParentSessionID != ""}})
		require.NoError(t, err)
	}
	c.sessions = session.NewService(env.q, env.conn)
	seed := func(owner, ref string) (jobbatch.ID, string) {
		t.Helper()
		root := jobbatch.ID("worker-stop-" + ref)
		payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-" + ref, SessionID: owner, Input: json.RawMessage(`{"file_path":"/tmp/stop-fixture"}`), RegistryID: env.registryID})
		require.NoError(t, err)
		spec := jobbatch.OneItem(jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload})
		_, err = env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{Root: root, Ref: ref, Spec: spec})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var count int
			_ = env.conn.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM batch_nodes WHERE root_id=? AND kind='task' AND state='running'`, root).Scan(&count)
			return count == 1
		}, time.Second, time.Millisecond)
		snap, err := env.store.Read(env.ctx, owner, string(root))
		require.NoError(t, err)
		return root, snap.Nodes[1].NodeID
	}
	seedTree := func(owner, ref string) (jobbatch.ID, []string) {
		t.Helper()
		makeTask := func(suffix string) jobbatch.Spec {
			payload, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: "call-" + ref + suffix, SessionID: owner, Input: json.RawMessage(`{"file_path":"/tmp/stop-fixture"}`), RegistryID: env.registryID})
			require.NoError(t, err)
			return jobbatch.OneItem(jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload})
		}
		root := jobbatch.ID("worker-stop-" + ref)
		spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: []jobbatch.Spec{makeTask("-a"), makeTask("-b")}}}
		_, err := env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{Root: root, Ref: ref, Spec: spec})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var count int
			_ = env.conn.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM batch_nodes WHERE root_id=? AND kind='task' AND state='running'`, root).Scan(&count)
			return count == 2
		}, time.Second, time.Millisecond)
		snap, err := env.store.Read(env.ctx, owner, string(root))
		require.NoError(t, err)
		ids := make([]string, 0, len(snap.Nodes))
		for _, n := range snap.Nodes {
			ids = append(ids, n.NodeID)
		}
		return root, ids
	}
	awaitStart := func() {
		t.Helper()
		select {
		case launch := <-executor.started:
			require.Equal(t, batchKindGeneric, launch.Task.Kind)
		case <-env.ctx.Done():
			t.Fatal("executor did not return an execution")
		}
	}
	assertNoControl := func(root jobbatch.ID) {
		t.Helper()
		var count int
		require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT COUNT(*) FROM batch_control_requests WHERE root_id=?`, root).Scan(&count))
		require.Zero(t, count)
	}
	stopOwnerTree := func(owner string, root jobbatch.ID) {
		t.Helper()
		_, err := c.Stop(env.ctx, owner, string(root), string(root))
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			var state string
			if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, root).Scan(&state); err != nil {
				return false
			}
			return state == string(jobbatch.StateCancelled)
		}, time.Second, time.Millisecond)
	}
	root, node := seed("worker", "own")
	awaitStart()
	require.Eventually(t, func() bool {
		var kind, state string
		if err := env.conn.QueryRowContext(env.ctx, `SELECT kind,state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&kind, &state); err != nil {
			return false
		}
		return kind == "task" && state == string(jobbatch.StateRunning)
	}, time.Second, time.Millisecond)
	_, err := c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var state string
		if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state); err != nil {
			return false
		}
		return state == string(jobbatch.StateCancelled)
	}, time.Second, time.Millisecond)

	root, node = seed("child", "descendant")
	awaitStart()
	_, err = c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var state string
		if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state); err != nil {
			return false
		}
		return state == string(jobbatch.StateCancelled)
	}, time.Second, time.Millisecond)

	root, nodes := seedTree("worker", "nested")
	awaitStart()
	awaitStart()
	_, err = c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), string(root))
	require.NoError(t, err)
	for _, id := range nodes {
		require.Eventually(t, func() bool {
			var state string
			if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, id).Scan(&state); err != nil {
				return false
			}
			return state == string(jobbatch.StateCancelled)
		}, time.Second, time.Millisecond)
	}

	for _, tc := range []struct{ name, owner string }{{"ancestor root", env.sessionID}, {"sibling", "sibling"}, {"unrelated", "stranger"}} {
		t.Run(tc.name, func(t *testing.T) {
			root, node := seed(tc.owner, "refused-"+strings.ReplaceAll(tc.name, " ", "-"))
			awaitStart()
			_, err := c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
			require.ErrorContains(t, err, "batch_stop")
			var state, owner string
			require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT state,owner_session_id FROM batch_nodes WHERE root_id=? AND node_id=root_id`, root).Scan(&state, &owner))
			require.Equal(t, tc.owner, owner)
			require.Equal(t, "running", state)
			var taskState string
			require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND kind='task'`, root).Scan(&taskState))
			require.Equal(t, string(jobbatch.StateRunning), taskState)
			assertNoControl(root)
			stopOwnerTree(tc.owner, root)
		})
	}

	root, node = seed("worker", "missing-owner")
	awaitStart()
	missing := testBatchSessions{Service: session.NewService(env.q, env.conn), rows: map[string]session.Session{}}
	cMissing := *c
	cMissing.sessions = missing
	_, err = cMissing.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
	require.ErrorContains(t, err, "owner")
	assertNoControl(root)
	stopOwnerTree("worker", root)

	root, node = seed("worker", "stale-node")
	awaitStart()
	_, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET runner_id='stale-runner' WHERE root_id=? AND node_id=?`, root, node)
	require.NoError(t, err)
	_, err = c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
	require.ErrorContains(t, err, "incarnation")
	assertNoControl(root)
	_, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET runner_id=? WHERE root_id=? AND node_id=?`, c.runnerID, root, node)
	require.NoError(t, err)
	stopOwnerTree("worker", root)

	root, node = seed("worker", "stale-root")
	awaitStart()
	_, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET runner_id='stale-runner' WHERE root_id=? AND node_id=root_id`, root)
	require.NoError(t, err)
	_, err = c.Stop(workerScopedStopContext(env.ctx), "worker", string(root), node)
	require.ErrorContains(t, err, "incarnation")
	assertNoControl(root)
	_, err = env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET runner_id=? WHERE root_id=? AND node_id=root_id`, c.runnerID, root)
	require.NoError(t, err)
	stopOwnerTree("worker", root)

	root, node = seed("worker", "orchestrator")
	awaitStart()
	_, err = c.Stop(env.ctx, env.sessionID, string(root), node)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var state string
		if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state); err != nil {
			return false
		}
		return state == string(jobbatch.StateCancelled)
	}, time.Second, time.Millisecond)

	root, node = seed("worker", "tool-own")
	awaitStart()
	inv := batchTestModelInv(env, "worker-tool")
	inv.SessionID, inv.WorkerStopScope = "worker", true
	workerTool := newBatchModelTools(c, inv, modelRegistry("view"))[4]
	input, err := json.Marshal(batchModelParams{Root: string(root), Node: node})
	require.NoError(t, err)
	response, err := workerTool.Run(context.WithValue(env.ctx, tools.SessionIDContextKey, "worker"), fantasy.ToolCall{Name: "batch_stop", Input: string(input)})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Contains(t, response.Content, "accepted")
	require.Eventually(t, func() bool {
		var state string
		if err := env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state); err != nil {
			return false
		}
		return state == string(jobbatch.StateCancelled)
	}, time.Second, time.Millisecond)

	root, node = seed(env.sessionID, "tool-ancestor")
	awaitStart()
	workerTool = newBatchModelTools(c, inv, modelRegistry("view"))[4]
	input, err = json.Marshal(batchModelParams{Root: string(root), Node: node})
	require.NoError(t, err)
	response, err = workerTool.Run(context.WithValue(env.ctx, tools.SessionIDContextKey, "worker"), fantasy.ToolCall{Name: "batch_stop", Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	var state string
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state))
	require.Equal(t, "running", state)
	assertNoControl(root)
	stopOwnerTree(env.sessionID, root)

	root, node = seed("stranger", "unscoped-stranger")
	awaitStart()
	_, err = c.Stop(env.ctx, "worker", string(root), node)
	require.ErrorContains(t, err, "batch_stop")
	var unscopedState string
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&unscopedState))
	require.Equal(t, "running", unscopedState)
	assertNoControl(root)
	stopOwnerTree("stranger", root)
}
