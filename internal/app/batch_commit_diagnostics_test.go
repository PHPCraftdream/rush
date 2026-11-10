package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

type batchDiagnosticPoisonError struct{}

func (batchDiagnosticPoisonError) Error() string { panic("error callback invoked") }
func (batchDiagnosticPoisonError) Unwrap() error { panic("unwrap callback invoked") }
func (batchDiagnosticPoisonError) As(any) bool   { panic("As callback invoked") }

func TestBatchCommitDiagnosticsDoesNotAcquireExhaustedPool(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writer, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(writer)) })
	writer.SetMaxOpenConns(1)
	diagnostic := batchCommitDiagnostics{writer: writer, baseline: writer.Stats()}
	held, err := writer.Conn(ctx)
	require.NoError(t, err)
	defer held.Close()
	secret := errors.New("private-hook-payload")
	runner, err := jobbatch.NewRunner(ctx, jobbatch.RunnerConfig{
		Limits:   jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 2, MaxNodes: 4},
		Executor: subprocessExecutor{}, ControlTimeout: time.Second, RecordRetention: 8,
		CommitTimeout: time.Second,
		CommitHook: func(ctx context.Context, _ jobbatch.Transition) error {
			jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseStatements)
			return secret
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = runner.Close(closeCtx)
	})
	_, fault := runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "test"})})
	require.ErrorIs(t, fault, secret)
	captured := make(chan []any, 1)
	go func() { captured <- diagnostic.snapshot(fault) }()
	var attrs []any
	select {
	case attrs = <-captured:
	case <-ctx.Done():
		t.Fatal("diagnostic snapshot waited for an exhausted connection pool")
	}
	values := make(map[string]any)
	for i := 0; i < len(attrs); i += 2 {
		values[attrs[i].(string)] = attrs[i+1]
	}
	require.Equal(t, "error", values["commit_kind"])
	require.EqualValues(t, 1, values["commit_sequence"])
	require.Equal(t, jobbatch.EventSubmit, values["commit_event"])
	require.Equal(t, jobbatch.CommitPhaseStatements, values["commit_phase"])
	require.Equal(t, time.Second, values["commit_budget"])
	require.Equal(t, 1, values["pool_max_open"])
	require.Equal(t, 1, values["pool_in_use"])
	require.NotContains(t, fmt.Sprint(attrs), secret.Error())
	require.Equal(t, diagnostic.baseline.WaitCount, writer.Stats().WaitCount, "diagnostics must never enter the pool queue")
	require.Equal(t, []any{"commit_kind", "unknown", "commit_phase", "unknown", "commit_error_class", "unknown"},
		(batchCommitDiagnostics{}).snapshot(batchDiagnosticPoisonError{}))
	require.Equal(t, "jobbatch: commit failed: unknown", batchCommitSafeFaultText(batchDiagnosticPoisonError{}))
	require.Equal(t, fault.Error(), batchCommitSafeFaultText(fault))
	require.Contains(t, attrs, "unknown", "an arbitrary hook error must not imply SQLite contention")
	require.ErrorIs(t, runner.Close(ctx), jobbatch.ErrCommitFailed)
}

func TestBatchCommitDiagnosticsClassifiesOnlyObservedSQLiteBusy(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	writer, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(writer)) })
	_, err = writer.ExecContext(ctx, "PRAGMA busy_timeout=0")
	require.NoError(t, err)
	var index int
	var name, path string
	require.NoError(t, writer.QueryRowContext(ctx, "PRAGMA database_list").Scan(&index, &name, &path))
	locker, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_txlock=immediate&_pragma=busy_timeout(0)", filepath.ToSlash(path)))
	require.NoError(t, err)
	defer locker.Close()
	held, err := locker.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer held.Rollback()
	runner, err := jobbatch.NewRunner(ctx, jobbatch.RunnerConfig{
		Limits:   jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 2, MaxNodes: 4},
		Executor: subprocessExecutor{}, ControlTimeout: time.Second, RecordRetention: 8,
		CommitTimeout: 10 * time.Second,
		CommitHook: func(ctx context.Context, _ jobbatch.Transition) error {
			jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseAcquire)
			tx, err := writer.BeginTx(ctx, nil)
			if err == nil {
				_ = tx.Rollback()
			}
			return err
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = runner.Close(closeCtx)
	})
	_, fault := runner.Submit(ctx, jobbatch.SubmitRequest{Root: "r", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "test"})})
	require.ErrorIs(t, fault, jobbatch.ErrCommitFailed)
	attrs := (batchCommitDiagnostics{writer: writer}).snapshot(fault)
	values := make(map[string]any)
	for i := 0; i < len(attrs); i += 2 {
		values[attrs[i].(string)] = attrs[i+1]
	}
	require.Equal(t, "error", values["commit_kind"])
	require.Equal(t, "sqlite_busy", values["commit_error_class"])
	require.Equal(t, jobbatch.CommitPhaseAcquire, values["commit_phase"])
	require.NotContains(t, fmt.Sprint(attrs), "SQLITE_BUSY", "raw driver text is not diagnostic metadata")
	require.ErrorIs(t, runner.Close(ctx), jobbatch.ErrCommitFailed)
}
