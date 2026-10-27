package app

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

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
			nodes := batchCLINodeOutcomes(t, a, root)
			require.Len(t, nodes, 3)
			require.Equal(t, string(jobbatch.StateCompleted), nodes[1].state)
			require.Equal(t, string(jobbatch.StateCompleted), nodes[2].state)
			require.NotEqual(t, nodes[1].id, nodes[2].id)
			firstID, secondID := root+".1", root+".2"
			if mode == jobbatch.ModeParallel && nodes[1].id == secondID {
				require.Contains(t, nodes[1].summary+"\n"+nodes[1].envelope, expectedSecond)
				require.Contains(t, nodes[2].summary+"\n"+nodes[2].envelope, expectedFirst)
			} else {
				require.Equal(t, firstID, nodes[1].id)
				require.Equal(t, secondID, nodes[2].id)
				require.Contains(t, nodes[1].summary+"\n"+nodes[1].envelope, expectedFirst)
				require.Contains(t, nodes[2].summary+"\n"+nodes[2].envelope, expectedSecond)
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
