package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type coordinatorStopRunningLeaf struct {
	countingTool
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (l *coordinatorStopRunningLeaf) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	l.startOnce.Do(func() { close(l.started) })
	select {
	case <-ctx.Done():
		close(l.cancelled)
		<-l.release
		return fantasy.ToolResponse{}, ctx.Err()
	case <-l.release:
		return fantasy.ToolResponse{}, nil
	}
}

func TestCoordinatorBatchStopRunningDelegatedChild(t *testing.T) {
	for _, legacyAvailable := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy-available", false: "legacy-absent"}[legacyAvailable], func(t *testing.T) {
			t.Setenv("RUSH_BATCH_RUNTIME", "1")
			leaf := &coordinatorStopRunningLeaf{countingTool: countingTool{name: "stop-running"}, started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
			logs := captureOwnerStopErrors(t)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(leaf.release) }) }
			t.Cleanup(release)
			ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
			defer cancel()
			parent, child := "batch-stop-running-parent", env.sessionID
			_, err := env.q.CreateSession(ctx, db.CreateSessionParams{ID: parent, Title: parent})
			require.NoError(t, err)
			sibling := "batch-stop-running-sibling"
			_, err = env.q.CreateSession(ctx, db.CreateSessionParams{ID: sibling, ParentSessionID: nullString(parent), Title: sibling})
			require.NoError(t, err)
			_, err = env.store.Reserve(ctx, session.BatchReservation{
				Owner: parent, Host: "parent-facts-host", RunnerID: env.install.Runtime.cfg.RunnerID, ToolCallID: "delegated-stop", InputHash: "parent-hash", Sequence: 1,
				Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none"}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("agent"), State: "running", ControlState: "none", ChildSessionID: nullString(child)}}}},
			})
			require.NoError(t, err)
			payload, err := json.Marshal(batchLeafPayload{Tool: leaf.name, ToolCallID: "detached-child", SessionID: child, Input: json.RawMessage(`{}`), RegistryID: env.registryID})
			require.NoError(t, err)
			root := batchRootID()
			_, err = env.install.RunnerHandle.Submit(ctx, jobbatch.SubmitRequest{Root: root, Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}, Ref: "detached-child"})
			require.NoError(t, err)
			rootID := string(root)
			won, err := env.install.Elector.ElectStarted(ctx, child, root)
			require.NoError(t, err)
			require.True(t, won)
			select {
			case <-leaf.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var state string
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&state))
			require.Equal(t, "running", state)
			var announced int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_nodes WHERE root_id=? AND node_id=root_id AND announced=1`, rootID).Scan(&announced))
			require.Equal(t, 1, announced)
			c := newCutoverCoordinator(t, env)
			cutoverBindFacts(t, c, env)
			ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
			require.NoError(t, err)
			controls := NewInstalledBatchModelControl(env.install.RunnerHandle, nil, env.store, env.live, c.sessions, env.install.Runtime.cfg.Host, env.install.Runtime.cfg.RunnerID, env.install.Runtime.cfg.Profile, nil)
			coordinatorBatchBindings.Store(c, &batchCoordinatorBinding{ingress: ingress, live: env.live, controls: controls})
			t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
			if !legacyAvailable {
				c.asyncJobs = nil
			}
			c.stopTree(sibling)
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&state))
			require.Equal(t, "running", state)
			var controlCount int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&controlCount))
			require.Zero(t, controlCount)
			select {
			case <-leaf.cancelled:
				t.Fatal("sibling stop cancelled running child")
			default:
			}
			c.stopTree(parent)
			c.stopTree(parent)
			c.stopTree(parent)
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&state))
			require.Equal(t, "cancelling", state)
			var selected int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_nodes WHERE root_id=? AND notice_selected=1`, rootID).Scan(&selected))
			require.Zero(t, selected)
			select {
			case <-leaf.cancelled:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var pending int
			var total int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0) FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&total, &pending))
			require.Equal(t, 1, total)
			require.Zero(t, pending)
			var answerCode string
			var answerSequence int64
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT answer_code, answer_sequence FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&answerCode, &answerSequence))
			require.Equal(t, "accepted", answerCode)
			require.Greater(t, answerSequence, int64(0))
			require.NotContains(t, logs.String(), "coordinator.Cancel:")
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&state))
			require.Equal(t, "cancelling", state)
			release()
			_, ok, err := env.install.Done.AwaitDone(ctx, child, root)
			require.NoError(t, err)
			require.True(t, ok)
			var terminal string
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&terminal))
			require.Equal(t, "cancelled", terminal)
			var wake int
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(wake),0) FROM batch_nodes WHERE root_id=? AND notice_selected=1`, rootID).Scan(&selected, &wake))
			require.GreaterOrEqual(t, selected, 1)
			require.Zero(t, wake)
			pulled, err := env.store.BatchPullNotices(ctx, env.messages, child, buildBatchNoticeMessageParams)
			require.NoError(t, err)
			require.NotEmpty(t, pulled)
			debt, err := env.store.BatchReactionVisibleDebtExists(ctx, child)
			require.NoError(t, err)
			require.False(t, debt)
			expired, err := env.store.BatchCountExpired(ctx, time.Now().Add(time.Hour).Unix())
			require.NoError(t, err)
			require.EqualValues(t, 1, expired, "only the settled child root is eligible; the fake parent is live")
			require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_control_requests WHERE root_id=? AND status='pending'`, rootID).Scan(&pending))
			require.Zero(t, pending)
			fresh := newCutoverCoordinator(t, env)
			cutoverBindFacts(t, fresh, env)
			if !legacyAvailable {
				fresh.asyncJobs = nil
			}
			require.False(t, fresh.arb.snapshot(child).Suspended)
			debt, err = coordinatorBatchCutoverFacts(fresh).SelectedDebtExists(ctx, child)
			require.NoError(t, err)
			require.False(t, debt)
		})
	}
}
