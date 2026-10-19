package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorBatchStopAnswersPendingAfterNaturalSettlement(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := &coordinatorStopRunningLeaf{countingTool: countingTool{name: "terminal-stop"}, started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(leaf.release) }) }
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	root := batchRootID()
	payload, err := json.Marshal(batchLeafPayload{Tool: leaf.name, ToolCallID: "terminal-child", SessionID: env.sessionID, Input: json.RawMessage(`{}`), RegistryID: env.registryID})
	require.NoError(t, err)
	_, err = env.install.RunnerHandle.Submit(ctx, jobbatch.SubmitRequest{Root: root, Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: payload}}, Ref: "terminal-child"})
	require.NoError(t, err)
	won, err := env.install.Elector.ElectStarted(ctx, env.sessionID, root)
	require.NoError(t, err)
	require.True(t, won)
	select {
	case <-leaf.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, c, env)
	ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
	require.NoError(t, err)
	coordinatorBatchBindings.Store(c, &batchCoordinatorBinding{ingress: ingress, live: env.live})
	t.Cleanup(func() { coordinatorBatchBindings.Delete(c) })
	originalRunner := env.install.Runtime.cfg.Runner
	runner := &retryStopRunner{batchRunnerHandle: originalRunner}
	env.install.Runtime.cfg.Runner = runner
	t.Cleanup(func() { env.install.Runtime.cfg.Runner = originalRunner })

	c.stopTree(env.sessionID)
	var total, pending int
	var requestID, finalRequestID string
	rootID := string(root)
	require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0) FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&total, &pending))
	require.Equal(t, 1, total)
	require.Equal(t, 1, pending)
	require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT request_id FROM batch_control_requests WHERE root_id=? AND status='pending'`, rootID).Scan(&requestID))
	require.Equal(t, 1, runner.calls)
	release()
	_, ok, err := env.install.Done.AwaitDone(ctx, env.sessionID, root)
	require.NoError(t, err)
	require.True(t, ok)
	var rootState string
	require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT state FROM batch_nodes WHERE root_id=? AND node_id=root_id`, rootID).Scan(&rootState))
	require.Equal(t, "completed", rootState)
	pulled, err := env.store.BatchPullNotices(ctx, env.messages, env.sessionID, buildBatchNoticeMessageParams)
	require.NoError(t, err)
	require.NotEmpty(t, pulled)
	debt, err := env.store.BatchReactionVisibleDebtExists(ctx, env.sessionID)
	require.NoError(t, err)
	require.False(t, debt)
	expired, err := env.store.BatchCountExpired(ctx, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.Zero(t, expired)

	c.stopTree(env.sessionID)
	c.stopTree(env.sessionID)
	require.Equal(t, 1, runner.calls)
	var answerCode string
	var answerSequence int64
	require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0), answer_code, answer_sequence FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&total, &pending, &answerCode, &answerSequence))
	require.Equal(t, 1, total)
	require.Zero(t, pending)
	require.NoError(t, env.conn.QueryRowContext(ctx, `SELECT request_id FROM batch_control_requests WHERE root_id=?`, rootID).Scan(&finalRequestID))
	require.Equal(t, requestID, finalRequestID)
	require.Equal(t, "accepted", answerCode)
	require.Greater(t, answerSequence, int64(0))
	expired, err = env.store.BatchCountExpired(ctx, time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.EqualValues(t, 1, expired)
}
