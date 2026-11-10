// Subprocess coverage uses the production exit path and a real exhausted
// writer pool. Helpers run only in CI, never as local verification.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// subprocessExecutor is a minimal Executor for the helper process.
type subprocessExecutor struct{}

func (subprocessExecutor) Start(context.Context, jobbatch.Launch, func(jobbatch.Result)) (jobbatch.Execution, error) {
	return nopExecution{}, nil
}

type nopExecution struct{}

func (nopExecution) Stop(context.Context) error { return nil }

func TestAppBatchFaultSubprocess(t *testing.T) {
	if os.Getenv("R11E_FAULT_HELPER") != "" {
		batchFaultSubprocessHelper()
		return
	}

	t.Parallel()
	for _, mode := range []string{"timeout", "error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			require.NoError(t, err)
			dir := t.TempDir()
			lockPath := filepath.Join(dir, "host.lock")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := platform.Command(ctx, exe, "-test.run=^TestAppBatchFaultSubprocess$")
			cmd.Env = append(os.Environ(),
				"R11E_FAULT_HELPER="+mode, "R11E_LOCK_PATH="+lockPath,
				"RUSH_GLOBAL_DATA="+filepath.Join(dir, "global-data"),
				"RUSH_GLOBAL_CONFIG="+filepath.Join(dir, "global-config"))
			start := time.Now()
			output, err := cmd.CombinedOutput()
			require.Error(t, err, "the helper must exit non-zero")
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr, "%s", output)
			require.Equal(t, BatchFaultExitCode, exitErr.ExitCode(), "output: %s", output)
			require.Less(t, time.Since(start), 30*time.Second, "fatal diagnostics must not wait for a pool connection")
			require.FileExists(t, lockPath, "the host lock must be retained")
			text := string(output)
			require.Equal(t, 1, strings.Count(text, "rush run: batch runtime fault:"), "%s", output)
			require.Equal(t, 1, strings.Count(text, "retained-host-identity"), "%s", output)
			require.NotContains(t, text, "private-hook-content")
			require.NotContains(t, text, "private-task-payload")
			require.Contains(t, text, "jobbatch: commit failed: "+mode)
			var diagnostic map[string]any
			count := 0
			for _, line := range strings.Split(text, "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "Batch runtime: commit fault - exiting process" {
					diagnostic = record
					count++
				}
			}
			require.Equal(t, 1, count, "%s", output)
			require.Equal(t, mode, diagnostic["commit_kind"])
			require.Equal(t, float64(1), diagnostic["commit_sequence"])
			require.Equal(t, string(jobbatch.EventSubmit), diagnostic["commit_event"])
			require.Equal(t, float64(BatchFaultExitCode), diagnostic["exit_code"])
			require.Equal(t, "100ms", diagnostic["containment"])
			require.Equal(t, "unknown", diagnostic["commit_error_class"])
			require.Equal(t, float64(1), diagnostic["pool_max_open"])
			require.Equal(t, float64(1), diagnostic["pool_in_use"])
			require.Equal(t, float64(0), diagnostic["pool_idle"])
			require.Contains(t, diagnostic, "commit_budget")
			require.Contains(t, diagnostic, "commit_elapsed")
			require.Contains(t, diagnostic, "commit_phase_elapsed")
			require.Contains(t, diagnostic, "pool_wait_count_since_runtime_start")
			require.Contains(t, diagnostic, "pool_wait_duration_since_runtime_start")
			if mode == "timeout" {
				// A watchdog may win before the hook is scheduled. Never infer
				// SQL lock contention or force hook entry through a timing bet.
				require.Contains(t, []string{"waiting_hook", "hook", "acquire_transaction"}, diagnostic["commit_phase"])
			} else {
				require.Equal(t, "statements", diagnostic["commit_phase"])
			}
		})
	}
}

func batchFaultSubprocessHelper() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	lockPath := os.Getenv("R11E_LOCK_PATH")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(2)
	}
	defer lock.Close()
	writer, err := db.Connect(context.Background(), filepath.Join(filepath.Dir(lockPath), "database"))
	if err != nil {
		os.Exit(3)
	}
	diagnostic := batchCommitDiagnostics{writer: writer, baseline: writer.Stats()}
	held, err := writer.Conn(context.Background())
	if err != nil {
		os.Exit(4)
	}
	defer held.Close()
	mode := os.Getenv("R11E_FAULT_HELPER")
	runner, err := jobbatch.NewRunner(context.Background(), jobbatch.RunnerConfig{
		CommitHook: func(ctx context.Context, _ jobbatch.Transition) error {
			if mode == "timeout" {
				jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseAcquire)
				tx, err := writer.BeginTx(ctx, nil)
				if err == nil {
					_ = tx.Rollback()
				}
				// Preserve coverage of a hook that never physically returns,
				// even after its database waiter has observed cancellation.
				<-make(chan struct{})
				return err
			}
			jobbatch.MarkCommitPhase(ctx, jobbatch.CommitPhaseStatements)
			if mode == "panic" {
				panic("private-hook-content")
			}
			return errors.New("private-hook-content")
		},
		CommitTimeout: time.Second, Executor: subprocessExecutor{},
		ControlTimeout: time.Second, RecordRetention: 8,
		Limits: jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 2, MaxNodes: 4},
	})
	if err != nil {
		os.Exit(5)
	}
	retained := false
	var rt *BatchRuntime
	rt = NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: 100 * time.Millisecond,
		RetainHostIdentity: func() {
			retained = true
			fmt.Fprintln(os.Stderr, "retained-host-identity")
		},
		BeforeExit: func(error) {
			if !rt.Fenced() || !retained || writer.Stats().InUse != 1 {
				os.Exit(6)
			}
		},
	})
	rt.commitDiagnostics = diagnostic
	rt.SetRunner(runner)
	rt.Start()
	_, submitErr := runner.Submit(context.Background(), jobbatch.SubmitRequest{
		Root: "r", Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "test", Payload: []byte(`"private-task-payload"`)}),
	})
	if !errors.Is(submitErr, jobbatch.ErrCommitFailed) {
		os.Exit(7)
	}
	select {} // The watcher alone owns retention and production os.Exit(70).
}
