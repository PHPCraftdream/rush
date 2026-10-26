package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// cliEngineCompletion starts instantly and reports Completed once.
type cliEngineCompletion struct{}

func (f *cliEngineCompletion) start(_ context.Context, _ batchLeafEnv, _ jobbatch.Launch, _ batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	go report(jobbatch.Result{State: jobbatch.StateCompleted})
	return cliEngineExecution{}, nil
}

type cliEngineExecution struct{}

func (cliEngineExecution) Stop(context.Context) error { return nil }

// TestBatchCLIEngineAcceptsParallelTranslatedTree drives the engine with a
// translated-style parallel tree; the parser-driven engine check is in cmd.
func TestBatchCLIEngineAcceptsParallelTranslatedTree(t *testing.T) {
	completion := &cliEngineCompletion{}
	runner, err := jobbatch.NewRunner(context.Background(), jobbatch.RunnerConfig{
		Executor: newBatchExecutor(batchLeafEnv{}, map[string]batchLeafFamily{
			batchKindArgv:    completion,
			batchKindGeneric: completion,
		}),
		Limits:          jobbatch.Limits{MaxConcurrent: 2, MaxDepth: 8, MaxNodes: 16},
		ControlTimeout:  time.Second,
		RecordRetention: 16,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, runner.Close(closeCtx))
	})

	payload := func(id string) []byte {
		raw, marshalErr := json.Marshal(batchLeafPayload{
			Tool: "run_command", ToolCallID: id, SessionID: "owner", Input: json.RawMessage(`{"program":"cmd","args":["/c","echo","x"]}`),
		})
		require.NoError(t, marshalErr)
		return raw
	}
	// The CLI parser's external defaults: mode parallel, on_fail EMPTY.
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: []jobbatch.Spec{
		{Task: &jobbatch.TaskSpec{Kind: batchKindArgv, Payload: payload("c1")}},
		{Task: &jobbatch.TaskSpec{Kind: batchKindArgv, Payload: payload("c2")}},
	}}}
	require.Empty(t, spec.Batch.OnFail)
	batch, err := runner.Submit(context.Background(), jobbatch.SubmitRequest{Root: "engine-parallel-root", Spec: spec, Ref: "engine-test"})
	require.NoError(t, err)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary, err := batch.Wait(closeCtx)
	require.NoError(t, err)
	require.Equal(t, jobbatch.StateCompleted, summary.State)
	require.EqualValues(t, 2, summary.Counts.Completed)
}
