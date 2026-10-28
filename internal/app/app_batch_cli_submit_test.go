package app

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchCLISessionValidatesExistingIDs(t *testing.T) {
	ctx := context.Background()
	a := newFullNewApp(t)

	foreignRoot := wsGitRepo(t)
	foreignSvc := session.NewServiceWithWorkspace(db.New(a.DB()), a.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignSvc.Create(ctx, "foreign")
	require.NoError(t, err)
	_, err = a.BatchCLISession(ctx, foreign.ID)
	require.ErrorIs(t, err, session.ErrForeignWorkspace)

	parent, err := a.Sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "tool-call", parent.ID, "child")
	require.NoError(t, err)
	_, err = a.BatchCLISession(ctx, child.ID)
	require.ErrorContains(t, err, "cannot continue a child session")

	_, err = a.BatchCLISession(ctx, "unknown-batch-session-id")
	require.Error(t, err)
	_, err = a.Sessions.Get(ctx, "unknown-batch-session-id")
	require.Error(t, err, "batch lookup must not create an unknown ID")
}

func TestBatchCLISessionFreshAndBusy(t *testing.T) {
	ctx := context.Background()
	a := newFullNewApp(t)
	fresh, err := a.BatchCLISession(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "rush batch", fresh.Title)

	busy, err := a.Sessions.Create(ctx, "busy")
	require.NoError(t, err)
	_, epoch, cancel, ok := a.AgentCoordinator.ReserveExclusive(ctx, busy.ID)
	require.True(t, ok)
	defer a.AgentCoordinator.ReleaseExclusive(busy.ID, epoch, cancel)
	_, err = a.BatchCLISession(ctx, busy.ID)
	require.ErrorIs(t, err, agent.ErrSessionBusy)
}

func TestAppBatchResultFailureAndSkippedLeaf(t *testing.T) {
	a := newFullNewApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess, err := a.BatchCLISession(ctx, "")
	require.NoError(t, err)
	a.Permissions.AutoApproveSession(sess.ID)
	badProgram := "rush-command-that-does-not-exist-92831"
	failed := jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{
		"tool": "run_command", "input": map[string]any{"program": badProgram},
	})}}
	second := jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{
		"tool": "run_command", "input": map[string]any{"program": "sh", "args": []string{"-c", "printf should-not-run"}},
	})}}
	if runtime.GOOS == "windows" {
		second = jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{
			"tool": "run_command", "input": map[string]any{"program": "cmd", "args": []string{"/c", "echo", "should-not-run"}},
		})}}
	}
	root, err := a.BatchSubmit(ctx, sess.ID, jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeSequential, OnFail: jobbatch.OnFailStop, Items: []jobbatch.Spec{failed, second}}})
	require.NoError(t, err)
	_, err = a.BatchWait(ctx, sess.ID, root)
	require.NoError(t, err)
	nodes, outputs, err := a.BatchResult(ctx, sess.ID, root)
	require.NoError(t, err)
	leaves := make(map[string]db.BatchNode, 2)
	for _, node := range nodes {
		if node.Kind == "task" {
			leaves[node.NodeID] = node
		}
	}
	require.Len(t, leaves, 2)
	first, secondNode := leaves[root+".1"], leaves[root+".2"]
	require.Equal(t, string(jobbatch.StateFailed), first.State)
	require.Equal(t, string(jobbatch.StateSkipped), secondNode.State)
	require.NotContains(t, outputs, secondNode.NodeID)
	require.NoError(t, a.BatchRelease(ctx, sess.ID, root))
}

func TestAppBatchSubmitRunCommandParallelAndSequential(t *testing.T) {
	a := newFullNewApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sess, err := a.BatchCLISession(ctx, "")
	require.NoError(t, err)
	a.Permissions.AutoApproveSession(sess.ID)
	var outputNumber atomic.Int64
	runCommand := func(label string) jobbatch.Spec {
		labelOutput := fmt.Sprintf("%s-%d", label, outputNumber.Add(1))
		command, program := []string{"/c", "echo", labelOutput}, "cmd"
		if runtime.GOOS != "windows" {
			program = "sh"
			command = []string{"-c", fmt.Sprintf("printf '%%s\\n' %q", labelOutput)}
		}
		return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{
			"tool":  "run_command",
			"input": map[string]any{"program": program, "args": command},
		})}}
	}
	for _, mode := range []jobbatch.Mode{jobbatch.ModeParallel, jobbatch.ModeSequential} {
		t.Run(string(mode), func(t *testing.T) {
			before := outputNumber.Load()
			firstLabel, secondLabel := "alpha", "beta"
			first, second := runCommand(firstLabel), runCommand(secondLabel)
			expectedFirst, expectedSecond := fmt.Sprintf("%s-%d", firstLabel, before+1), fmt.Sprintf("%s-%d", secondLabel, before+2)
			items := []jobbatch.Spec{first, second}
			spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: mode, Items: items}}
			if mode == jobbatch.ModeSequential {
				spec.Batch.OnFail = jobbatch.OnFailStop
			}
			root, err := a.BatchSubmit(ctx, sess.ID, spec)
			require.NoError(t, err)
			snapshot, err := a.BatchWait(ctx, sess.ID, root)
			require.NoError(t, err)
			require.Equal(t, string(jobbatch.StateCompleted), snapshot.State)
			nodes, outputs, err := a.BatchResult(ctx, sess.ID, root)
			require.NoError(t, err)
			outcomes := batchCLINodeOutcomes(t, a, root)
			require.Len(t, outcomes, 3)
			require.Equal(t, string(jobbatch.StateCompleted), outcomes[1].state)
			require.Equal(t, string(jobbatch.StateCompleted), outcomes[2].state)
			leaves := make(map[string]db.BatchNode, 2)
			for _, node := range nodes {
				if node.Kind == "task" {
					leaves[node.NodeID] = node
				}
			}
			require.Len(t, leaves, 2)
			require.Contains(t, outputs, outcomes[1].id)
			require.Contains(t, outputs, outcomes[2].id)
			require.Equal(t, string(jobbatch.StateCompleted), leaves[outcomes[1].id].State)
			require.Equal(t, string(jobbatch.StateCompleted), leaves[outcomes[2].id].State)
			require.Contains(t, outputs[outcomes[1].id], expectedFirst)
			require.Contains(t, outputs[outcomes[2].id], expectedSecond)
			require.NoError(t, a.BatchRelease(ctx, sess.ID, root))
			// After release the outcomes still report; output comes from the
			// cache while it lasts and is omitted once evicted.
			released, releasedOutputs, err := a.BatchResult(ctx, sess.ID, root)
			require.NoError(t, err)
			require.Len(t, released, len(nodes))
			for id, out := range releasedOutputs {
				require.Equal(t, outputs[id], out)
			}
		})
	}
}

type batchCLIOutcome struct {
	id, state, code, summary, envelope string
}

func batchCLINodeOutcomes(t *testing.T, a *App, root string) []batchCLIOutcome {
	t.Helper()
	rows, err := a.DB().QueryContext(context.Background(), `SELECT node_id, state, COALESCE(outcome_code, ''), COALESCE(outcome_summary, ''), COALESCE(response_envelope, '') FROM batch_nodes WHERE root_id = ? ORDER BY node_id`, root)
	require.NoError(t, err)
	defer rows.Close()
	var outcomes []batchCLIOutcome
	for rows.Next() {
		var outcome batchCLIOutcome
		require.NoError(t, rows.Scan(&outcome.id, &outcome.state, &outcome.code, &outcome.summary, &outcome.envelope))
		outcomes = append(outcomes, outcome)
	}
	require.NoError(t, rows.Err())
	return outcomes
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}
