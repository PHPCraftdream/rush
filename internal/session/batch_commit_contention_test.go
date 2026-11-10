package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

func batchDiagnosticRunnerCleanup(t *testing.T, runner *jobbatch.Runner) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = runner.Close(ctx)
	})
}

// This is a real SQLite writer lock, not writer-pool exhaustion. The second
// handle belongs only to the fixture; production still has exactly one writer.
func TestBatchCommitExternalWriterLockPreservesTypedBusy(t *testing.T) {
	for _, operation := range []string{"submit", "projection"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			env := newCommitHookEnv(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			env.ctx = ctx
			require.NoError(t, db.PdaBatchEnsure(ctx, env.conn))
			// Only this temporary connection's busy policy changes. A held lock
			// must return a typed error, not race a short watchdog deadline.
			_, err := env.conn.ExecContext(ctx, "PRAGMA busy_timeout=0")
			require.NoError(t, err)
			var index int
			var name, path string
			require.NoError(t, env.conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&index, &name, &path))
			locker, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(0)", filepath.ToSlash(path)))
			require.NoError(t, err)
			defer locker.Close()
			locker.SetMaxOpenConns(1)
			held, err := locker.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer held.Rollback()
			hook := env.adapter()
			runner, executor, _ := commitHookRunner(t, hook)
			var commitErr error
			if operation == "submit" {
				_, commitErr = runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: commitHookBatch(jobbatch.NotifyAll, commitHookTask())})
			} else {
				commitErr = runner.CapacityAvailable(ctx, "")
			}
			require.ErrorIs(t, commitErr, jobbatch.ErrCommitFailed)
			var fault *jobbatch.CommitFault
			require.ErrorAs(t, commitErr, &fault)
			require.Equal(t, "error", fault.Details().Kind, "an observed busy error is not a watchdog timeout")
			require.Equal(t, jobbatch.CommitPhaseAcquire, fault.Details().Phase, "BEGIN IMMEDIATE acquires SQLite's writer lock")
			var busy *sqlite.Error
			require.ErrorAs(t, commitErr, &busy)
			require.Equal(t, 5, busy.Code()&0xff)
			require.Equal(t, "jobbatch: commit failed: error", commitErr.Error())
			require.Empty(t, executor.starts)
			require.NoError(t, held.Rollback())
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
			require.ErrorIs(t, runner.Close(ctx), jobbatch.ErrCommitFailed)
		})
	}
}

func TestBatchCommitReleasedPoolCommitsOnceBeforeDispatch(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	env := newCommitHookEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	env.ctx = ctx
	require.NoError(t, db.PdaBatchEnsure(ctx, env.conn))
	held, err := env.conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer held.Rollback()
	before := env.conn.Stats()
	durable := make(chan error, 1)
	publish := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(publish) }) }
	hook := env.adapter()
	var submissions atomic.Int32
	runner, executor, _ := commitHookRunner(t, func(ctx context.Context, tr jobbatch.Transition) error {
		if tr.Event.Type != jobbatch.EventSubmit {
			return hook(ctx, tr)
		}
		submissions.Add(1)
		err := hook(ctx, tr)
		durable <- err
		select {
		case <-publish:
		case <-ctx.Done():
		}
		return err
	})
	// Registered after the runner cleanup so failure cannot strand the hook.
	t.Cleanup(release)
	type reply struct {
		batch *jobbatch.Batch
		err   error
	}
	replied := make(chan reply, 1)
	go func() {
		b, err := runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: commitHookBatch(jobbatch.NotifyAll, commitHookTask())})
		replied <- reply{batch: b, err: err}
	}()
	// Observe the real database/sql queue, not a sleep or presumed hook entry.
	require.Eventually(t, func() bool { return env.conn.Stats().WaitCount > before.WaitCount },
		5*time.Second, time.Millisecond, "commit must reach the exhausted writer pool")
	require.Empty(t, executor.starts)
	require.NoError(t, held.Rollback())
	require.NoError(t, commitHookReceive(t, ctx, durable))
	// The owning-layer post-commit barrier makes durability independently
	// observable before the runner may publish or dispatch the transition.
	require.EqualValues(t, 1, submissions.Load())
	require.EqualValues(t, 1, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE node_id='r'"))
	seq, event, _ := commitHookCheckpoint(t, env.conn, env.id.Runner)
	require.EqualValues(t, 1, seq)
	require.Equal(t, string(jobbatch.EventSubmit), event)
	require.Empty(t, executor.starts)
	select {
	case <-replied:
		t.Fatal("submit replied before durable hook acknowledgement")
	default:
	}
	release()
	answer := commitHookReceive(t, ctx, replied)
	require.NoError(t, answer.err)
	attempt := commitHookAccept(t, ctx, runner, executor, nil)
	commitHookFinish(t, ctx, runner, attempt, jobbatch.Result{State: jobbatch.StateCompleted})
	commitHookWait(t, ctx, answer.batch)
	require.EqualValues(t, 1, submissions.Load(), "contention release must not retry Submit")
	require.EqualValues(t, 1, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes WHERE node_id='r'"))
	require.Empty(t, executor.starts, "durable transition must dispatch exactly once")
	require.NoError(t, runner.FaultErr())
	require.NoError(t, runner.Close(ctx))
}

// Owning-layer seam, not a SQLite lock reproduction: return an error after a
// successful store commit to freeze the last phase without a scheduling race.
func TestBatchCommitFinalPhaseForReservationAndRootlessProjection(t *testing.T) {
	for _, operation := range []string{"submit", "projection"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			env := newCommitHookEnv(t)
			hook := env.adapter()
			sentinel := errors.New("post-commit test barrier")
			runner, executor, ctx := commitHookRunner(t, func(ctx context.Context, tr jobbatch.Transition) error {
				if err := hook(ctx, tr); err != nil {
					return err
				}
				return sentinel
			})
			var err error
			if operation == "submit" {
				_, err = runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: commitHookBatch(jobbatch.NotifyAll, commitHookTask())})
			} else {
				err = runner.CapacityAvailable(ctx, "")
			}
			require.ErrorIs(t, err, sentinel)
			var fault *jobbatch.CommitFault
			require.ErrorAs(t, err, &fault)
			require.Equal(t, jobbatch.CommitPhaseCommit, fault.Details().Phase)
			require.EqualValues(t, 1, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"), "rootless projection also commits its checkpoint")
			require.Empty(t, executor.starts)
			require.ErrorIs(t, runner.Close(ctx), jobbatch.ErrCommitFailed)
		})
	}
}

func TestBatchCommitStatementFailurePhaseIsAtomic(t *testing.T) {
	for _, operation := range []string{"submit", "projection"} {
		t.Run(operation, func(t *testing.T) {
			t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
			t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
			env := newCommitHookEnv(t)
			_, err := env.conn.ExecContext(t.Context(), `CREATE TRIGGER reject_checkpoint BEFORE INSERT ON batch_runner_checkpoints
				BEGIN SELECT RAISE(ABORT, 'private-statement-error'); END`)
			require.NoError(t, err)
			runner, executor, ctx := commitHookRunner(t, env.adapter())
			if operation == "submit" {
				_, err = runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: commitHookBatch(jobbatch.NotifyAll, commitHookTask())})
			} else {
				err = runner.CapacityAvailable(ctx, "")
			}
			require.ErrorIs(t, err, jobbatch.ErrCommitFailed)
			var fault *jobbatch.CommitFault
			require.ErrorAs(t, err, &fault)
			require.Equal(t, "error", fault.Details().Kind)
			require.Equal(t, jobbatch.CommitPhaseStatements, fault.Details().Phase)
			require.NotContains(t, err.Error(), "private-statement-error")
			var sqlErr *sqlite.Error
			require.ErrorAs(t, err, &sqlErr)
			require.Empty(t, executor.starts)
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_nodes"))
			require.Zero(t, commitHookCount(t, env.conn, "SELECT count(*) FROM batch_runner_checkpoints"))
			require.ErrorIs(t, runner.Close(ctx), jobbatch.ErrCommitFailed)
		})
	}
}
