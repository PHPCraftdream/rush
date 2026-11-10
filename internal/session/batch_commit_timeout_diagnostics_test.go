package session

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchCommitTimeoutIdentifiesExhaustedWriterPool(t *testing.T) {
	for _, operation := range []string{"submit", "projection", "prepare"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			env := newCommitHookEnv(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			env.ctx = ctx
			env.conn.SetMaxOpenConns(1)
			if operation != "prepare" {
				require.NoError(t, db.PdaBatchEnsure(env.ctx, env.conn))
			}
			before := env.conn.Stats()
			held, err := env.conn.BeginTx(env.ctx, nil)
			require.NoError(t, err)
			defer held.Rollback()
			hookReturned := make(chan error, 1)
			hook := env.adapter()
			executor := &commitHookExecutor{starts: make(chan *commitHookAttempt, 8)}
			runner, err := jobbatch.NewRunner(env.ctx, jobbatch.RunnerConfig{
				Limits:   jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 4, MaxNodes: 16},
				Executor: executor, ControlTimeout: time.Second, RecordRetention: 16,
				CommitTimeout: time.Second,
				CommitHook: func(ctx context.Context, transition jobbatch.Transition) error {
					err := hook(ctx, transition)
					hookReturned <- err
					return err
				},
			})
			require.NoError(t, err)
			batchDiagnosticRunnerCleanup(t, runner)
			var commitErr error
			if operation != "projection" {
				_, commitErr = runner.Submit(env.ctx, jobbatch.SubmitRequest{Root: "r", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "test"})})
			} else {
				commitErr = runner.CapacityAvailable(env.ctx, "")
			}
			require.ErrorIs(t, commitErr, jobbatch.ErrCommitFailed)
			var fault *jobbatch.CommitFault
			require.ErrorAs(t, commitErr, &fault)
			require.Equal(t, "timeout", fault.Details().Kind)
			phase := jobbatch.CommitPhaseAcquire
			if operation == "prepare" {
				phase = jobbatch.CommitPhasePrepare
			}
			require.Equal(t, phase, fault.Details().Phase)
			select {
			case err := <-hookReturned:
				require.ErrorIs(t, err, context.Canceled, "pool waiter must observe watchdog cancellation")
			case <-env.ctx.Done():
				t.Fatal("commit hook did not leave the exhausted pool")
			}
			after := env.conn.Stats()
			require.Equal(t, 1, after.MaxOpenConnections)
			require.Equal(t, 1, after.InUse, "the deliberately held transaction still owns the only connection")
			require.Greater(t, after.WaitCount, before.WaitCount)
			require.Empty(t, executor.starts)
			require.NoError(t, held.Rollback())
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
			require.ErrorIs(t, runner.Close(env.ctx), jobbatch.ErrCommitFailed)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("operation=%s before=%+v after=%+v fault=%+v", operation, before, after, fault.Details())
				}
			})
		})
	}
}
