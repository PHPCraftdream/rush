package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

type testBatchSessions struct {
	session.Service
	rows map[string]session.Session
}

func (s testBatchSessions) Get(_ context.Context, id string) (session.Session, error) {
	v, ok := s.rows[id]
	if !ok {
		return session.Session{}, fmt.Errorf("missing session")
	}
	return v, nil
}

func (s testBatchSessions) ListSubSessions(_ context.Context, parent string) ([]session.Session, error) {
	var out []session.Session
	for _, v := range s.rows {
		if v.ParentSessionID == parent {
			out = append(out, v)
		}
	}
	return out, nil
}

func batchTestSessions(owner string) session.Service {
	return testBatchSessions{rows: map[string]session.Session{owner: {ID: owner}, "child": {ID: "child", ParentSessionID: owner}, "grandchild": {ID: "grandchild", ParentSessionID: "child"}, "stranger": {ID: "stranger"}}}
}

func batchControlProfile(t *testing.T) *BatchProfile {
	t.Helper()
	p, err := NewBatchProfileDefault()
	require.NoError(t, err)
	return p
}

func batchOwnerTree(t *testing.T, env *batchInventoryEnv, root jobbatch.ID, children []jobbatch.Spec, ref string) session.BatchSnapshot {
	t.Helper()
	var identify func(*jobbatch.Spec, string)
	identify = func(spec *jobbatch.Spec, path string) {
		if spec.Task != nil {
			var payload batchLeafPayload
			require.NoError(t, json.Unmarshal(spec.Task.Payload, &payload))
			payload.SessionID = env.sessionID
			payload.ToolCallID = fmt.Sprintf("%s-%s", ref, path)
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			spec.Task.Payload = raw
		}
		if spec.Batch != nil {
			for i := range spec.Batch.Items {
				identify(&spec.Batch.Items[i], fmt.Sprintf("%s-%d", path, i))
			}
		}
	}
	for i := range children {
		identify(&children[i], fmt.Sprintf("%d", i))
	}
	_, err := env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{Root: root, Spec: jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: children}}, Ref: ref})
	require.NoError(t, err)
	s, err := env.store.Read(env.ctx, env.sessionID, string(root))
	require.NoError(t, err)
	return s
}

func batchOwnedControl(t *testing.T, env *batchInventoryEnv, p *BatchProfile) *installedBatchModelControl {
	t.Helper()
	runner, ok := any(env.install.RunnerHandle).(batchModelRunner)
	require.True(t, ok, "installed runner handle must expose model controls")
	return &installedBatchModelControl{runner: runner, store: env.store, live: session.NewBatchLiveReader(db.New(env.conn), ""), sessions: batchTestSessions(env.sessionID), host: env.install.Runtime.cfg.Host, runnerID: env.install.Runtime.cfg.RunnerID, profile: p, maxDepth: 8, maxNodes: 64}
}

func batchTestModelInv(env *batchInventoryEnv, call string) BatchInvocation {
	return BatchInvocation{SessionID: env.sessionID, MessageID: "msg", CallID: call, RegistryID: env.registryID, Origin: "coordinator", WaiterRank: batchGroupRanks[batchGroupDefault], Delivery: batchDeliveryChoice{Mode: batchDeliveryDetached}, Policy: batchLeafPolicyContext{RegistryID: env.registryID, RestrictedToolset: "view"}}
}

func TestInstalledModelControlRecursiveDefaultsAndPinnedPolicy(t *testing.T) {
	inv := BatchInvocation{SessionID: "owner", CallID: "call", RegistryID: "reg", Policy: batchLeafPolicyContext{RegistryID: "reg", RestrictedToolset: "view"}}
	spec, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{"path":"x"}`)}, {Batch: &batchModelSpec{Mode: "sequential", Notify: "each", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{"path":"y"}`)}}}}}}, inv, modelRegistry("view"))
	require.NoError(t, err)
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify, "explicit omitted notify defaults to all")
	require.Empty(t, spec.Batch.OnFail)
	var p batchLeafPayload
	require.NoError(t, json.Unmarshal(spec.Batch.Items[0].Task.Payload, &p))
	require.Equal(t, "call-item-0", p.ToolCallID)
	var nestedPayload batchLeafPayload
	require.NoError(t, json.Unmarshal(spec.Batch.Items[1].Batch.Items[0].Task.Payload, &nestedPayload))
	require.NotEqual(t, p.ToolCallID, nestedPayload.ToolCallID)
	require.Equal(t, "call-item-1.0", nestedPayload.ToolCallID)
}

func TestInstalledModelControlRejectsNestedControlAndUnavailableLeaf(t *testing.T) {
	inv := BatchInvocation{SessionID: "owner", CallID: "c", RegistryID: "reg", Policy: batchLeafPolicyContext{RegistryID: "reg", RestrictedToolset: "view"}}
	_, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Batch: &batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "batch_stop", Input: json.RawMessage(`{}`)}}}}}}, inv, modelRegistry("view"))
	require.ErrorContains(t, err, "cannot be a batch leaf")
	_, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: "edit", Input: json.RawMessage(`{}`)}}}, inv, modelRegistry("view"))
	require.ErrorContains(t, err, "unavailable")
}

func TestInstalledModelControlRunAdmissionAndDelivery(t *testing.T) {
	for _, notify := range []string{"", "each"} {
		t.Run("notify-"+notify, func(t *testing.T) {
			env := newBatchInventoryEnv(t, "view")
			c := NewInstalledBatchModelControl(env.install.RunnerHandle, env.install.Delivery, env.store, session.NewBatchLiveReader(db.New(env.conn), ""), batchTestSessions(env.sessionID), env.install.Runtime.cfg.Host, env.install.Runtime.cfg.RunnerID, batchControlProfile(t), nil).(*installedBatchModelControl)
			inv := batchTestModelInv(env, "real-run-"+notify)
			reg, err := env.registries.Resolve(env.registryID)
			require.NoError(t, err)
			root, err := c.Run(env.ctx, inv, reg, batchModelSpec{Mode: "parallel", Notify: notify, Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{}`)}}})
			require.NoError(t, err)
			require.NotEmpty(t, root)
			snap, err := env.store.Read(env.ctx, env.sessionID, string(root))
			require.NoError(t, err)
			require.Equal(t, env.sessionID, snap.Root.OwnerSessionID)
			require.Equal(t, "real-run-"+notify, snap.Root.ToolCallID.String)
			if notify == "each" {
				require.Equal(t, "each", snap.Root.Notify.String)
			} else {
				require.Equal(t, "all", snap.Root.Notify.String)
			}
		})
	}
}

func TestInstalledModelControlRunMatchesOneItemNotifyAll(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	task := batchTestLeaf(batchKindGeneric, "view", "").Task
	var payload batchLeafPayload
	require.NoError(t, json.Unmarshal(task.Payload, &payload))
	payload.SessionID, payload.ToolCallID = env.sessionID, "oneitem-call"
	task.Payload, _ = json.Marshal(payload)
	root := jobbatch.ID("oneitem-notify-root")
	_, err := env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{Root: root, Ref: "oneitem-call", Spec: jobbatch.OneItem(*task)})
	require.NoError(t, err)
	snap, err := env.store.Read(env.ctx, env.sessionID, string(root))
	require.NoError(t, err)
	require.Equal(t, "all", snap.Root.Notify.String)
}

func TestInstalledModelControlNestedAddAndOwnedAncestor(t *testing.T) {
	// The Add fence rejects terminal roots, so the seed item must stay live
	// while Add runs: hold the executor (the gating pattern the other
	// live-required control tests use) or the instant-settle default settles
	// the one-item root before Add and the precondition legitimately rejects.
	gate := newGatingInstallExecutor("held")
	t.Cleanup(gate.letGo)
	env := newBatchInventoryEnvWithExecutor(t, "view", gate)
	root := jobbatch.ID("ancestor-root-r13")
	task := batchTestLeaf(batchKindGeneric, "view", "").Task
	_ = batchOwnerTree(t, env, root, []jobbatch.Spec{{Task: task}}, "ancestor-call")
	rows := map[string]session.Session{env.sessionID: {ID: env.sessionID}, "direct-child": {ID: "direct-child", ParentSessionID: env.sessionID}, "grandchild": {ID: "grandchild", ParentSessionID: "direct-child"}, "other": {ID: "other"}}
	c := batchOwnedControl(t, env, batchControlProfile(t))
	c.sessions = testBatchSessions{rows: rows}
	inv := batchTestModelInv(env, "nested-add")
	inv.SessionID = "grandchild"
	items := []batchModelItem{{Batch: &batchModelSpec{Mode: "sequential", Items: []batchModelItem{{Tool: "view", Input: json.RawMessage(`{}`)}}}}}
	ids, err := c.Add(env.ctx, "grandchild", string(root), "", inv, modelRegistry("view"), items)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	_, err = c.Add(env.ctx, "other", string(root), "", inv, modelRegistry("view"), items)
	require.Error(t, err)
}

func TestInstalledModelControlStatusOwnershipAndIncomplete(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	root := jobbatch.ID("status-root")
	task := batchTestLeaf(batchKindGeneric, "view", "").Task
	snap := batchOwnerTree(t, env, root, []jobbatch.Spec{{Task: task}}, "status-call")
	c := batchOwnedControl(t, env, batchControlProfile(t))
	got, err := c.StatusNode(env.ctx, "child", string(root), snap.Nodes[1].NodeID)
	require.NoError(t, err)
	require.Contains(t, got.(map[string]any), "complete")
	got, err = c.StatusNode(env.ctx, "stranger", string(root), snap.Nodes[1].NodeID)
	require.NoError(t, err)
	require.False(t, got.(map[string]any)["terminal"].(bool))
}

func TestInstalledModelControlOutputGapAndLimit(t *testing.T) {
	gate := newGatingInstallExecutor(strings.Repeat("x", batchModelOutputLimit+4))
	env := newBatchInventoryEnvWithExecutor(t, "view", gate)
	started := make(chan struct{}, 1)
	gate.started = started
	root := jobbatch.ID("output-root-r13")
	task := batchTestLeaf(batchKindGeneric, "view", "").Task
	snap := batchOwnerTree(t, env, root, []jobbatch.Spec{{Task: task}}, "output-call")
	node := snap.Nodes[1].NodeID
	select {
	case <-started:
	case <-env.ctx.Done():
		t.Fatal("executor did not enter held Start")
	}
	gate.letGo()
	require.Eventually(t, func() bool {
		var state string
		_ = env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, node).Scan(&state)
		return state == "completed"
	}, time.Second, time.Millisecond)
	c := batchOwnedControl(t, env, batchControlProfile(t))
	got, err := c.Output(env.ctx, env.sessionID, string(root), node, 0)
	require.NoError(t, err)
	chunk := got.(jobbatch.OutputChunk)
	require.Len(t, chunk.Data, batchModelOutputLimit)
	require.Equal(t, uint64(batchModelOutputLimit), chunk.Next)
	require.True(t, chunk.Truncated)
	require.False(t, chunk.EOF)
	toolSet := newBatchModelTools(c, batchTestModelInv(env, "output-tool"), modelRegistry("view"))
	params, err := json.Marshal(batchModelParams{Root: string(root), Node: node})
	require.NoError(t, err)
	resp, err := toolSet[3].Run(context.WithValue(env.ctx, tools.SessionIDContextKey, env.sessionID), fantasy.ToolCall{Name: "batch_output", Input: string(params)})
	require.NoError(t, err)
	require.Contains(t, resp.Content, `"data":"`+strings.Repeat("x", 32))
}

func TestInstalledModelControlAddNotifyEachOutbox(t *testing.T) {
	gate := newGatingInstallExecutor("added-result")
	t.Cleanup(gate.letGo)
	env := newBatchInventoryEnvWithExecutor(t, "view", gate)
	started := make(chan struct{}, 2)
	gate.started = started
	root := jobbatch.ID("notify-each-add-root")
	task := batchTestLeaf(batchKindGeneric, "view", "").Task
	// The root's explicit NotifyEach mode must survive reservation, then govern
	// notices selected for items appended later through the model control.
	payload := []byte(`{}`)
	task.Payload = payload
	payloadForOwner := func(id string) []byte {
		p, err := json.Marshal(batchLeafPayload{Tool: "view", ToolCallID: id, SessionID: env.sessionID, Input: json.RawMessage(`{}`), RegistryID: env.registryID})
		require.NoError(t, err)
		return p
	}
	task.Payload = payloadForOwner("seed-call")
	_, err := env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{Root: root, Ref: "seed-call", Spec: jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyEach, Items: []jobbatch.Spec{{Task: task}}}}})
	require.NoError(t, err)
	select {
	case <-started:
	case <-env.ctx.Done():
		t.Fatal("seed task did not enter held Start")
	}
	c := batchOwnedControl(t, env, batchControlProfile(t))
	inv := batchTestModelInv(env, "later-add-call")
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	ids, err := c.Add(env.ctx, env.sessionID, string(root), "", inv, reg, []batchModelItem{{Tool: "view", Input: json.RawMessage(`{}`)}})
	require.NoError(t, err)
	require.Len(t, ids, 1)
	select {
	case <-started:
	case <-env.ctx.Done():
		t.Fatal("added task did not enter held Start")
	}
	gate.letGo()
	addedID := string(ids[0])
	require.Eventually(t, func() bool {
		var state string
		_ = env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=?`, root, addedID).Scan(&state)
		return state == "completed"
	}, time.Second, time.Millisecond)
	var selected, wake int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT notice_selected,wake FROM batch_nodes WHERE root_id=? AND node_id=?`, root, addedID).Scan(&selected, &wake))
	require.Equal(t, int64(1), selected)
	require.Equal(t, int64(1), wake)
}

func TestInstalledModelControlStopRootAndDescendant(t *testing.T) {
	for _, target := range []string{"root", "node"} {
		t.Run(target, func(t *testing.T) {
			gate := newGatingInstallExecutor("held")
			t.Cleanup(gate.letGo)
			env := newBatchInventoryEnvWithExecutor(t, "view", gate)
			started := make(chan struct{}, 1)
			gate.started = started
			root := jobbatch.ID("stop-root-r13-" + target)
			task := batchTestLeaf(batchKindGeneric, "view", "").Task
			snap := batchOwnerTree(t, env, root, []jobbatch.Spec{{Task: task}}, "stop-call-"+target)
			select {
			case <-started:
			case <-env.ctx.Done():
				t.Fatal("executor did not enter held Start")
			}
			node := snap.Nodes[1].NodeID
			if target == "root" {
				node = string(root)
			}
			c := batchOwnedControl(t, env, batchControlProfile(t))
			got, err := c.Stop(env.ctx, env.sessionID, string(root), node)
			require.NoError(t, err)
			result := got.(map[string]any)
			require.True(t, result["accepted"].(bool))
			require.False(t, result["terminal"].(bool))
			var status string
			require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT status FROM batch_control_requests WHERE request_id=?`, result["request_id"]).Scan(&status))
			require.Equal(t, "answered", status)
			var answerCode sql.NullString
			require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT answer_code FROM batch_control_requests WHERE request_id=?`, result["request_id"]).Scan(&answerCode))
			require.True(t, answerCode.Valid)
			require.Equal(t, "accepted", answerCode.String)
			var answerSequence sql.NullInt64
			require.NoError(t, env.conn.QueryRowContext(env.ctx, `SELECT answer_sequence FROM batch_control_requests WHERE request_id=?`, result["request_id"]).Scan(&answerSequence))
			require.True(t, answerSequence.Valid)
			require.Positive(t, answerSequence.Int64)
			gate.letGo()
			require.Eventually(t, func() bool {
				var state string
				_ = env.conn.QueryRowContext(env.ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, root).Scan(&state)
				return terminalBatchStoreState(state)
			}, time.Second, time.Millisecond)
		})
	}
}
