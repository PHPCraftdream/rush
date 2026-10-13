// Subprocess test: a real *jobbatch.Runner with a never-returning commit
// hook faults, and the process exits 70 through the production os.Exit while
// the lock file stays in place. One subprocess per test (the handler is
// shared by all host kinds — CLI, server, SDK; see app_batch_doc.go).

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	if os.Getenv("R11E_FAULT_HELPER") == "1" {
		lockPath := os.Getenv("R11E_LOCK_PATH")
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(2)
		}
		defer lock.Close() // unreachable on the exit-70 path; kept for lint

		neverReturn := make(chan struct{})
		runner, err := jobbatch.NewRunner(context.Background(), jobbatch.RunnerConfig{
			CommitHook: func(context.Context, jobbatch.Transition) error {
				<-neverReturn // never-returning commit hook
				return nil
			},
			CommitTimeout:   50 * time.Millisecond,
			Executor:        subprocessExecutor{},
			ControlTimeout:  time.Second,
			RecordRetention: 8,
			Limits: jobbatch.Limits{
				MaxConcurrent: 1, MaxDepth: 2, MaxNodes: 4,
			},
		})
		if err != nil {
			os.Exit(3)
		}

		// The CLI host shape (what R11F installs): runtime with the real
		// Runner, production os.Exit, small containment bound for cheapness.
		rt := NewBatchRuntime(BatchRuntimeConfig{ContainmentWait: 100 * time.Millisecond})
		rt.SetRunner(runner)
		rt.Start()

		if _, err := runner.Submit(context.Background(), jobbatch.SubmitRequest{
			Root: "r",
			Spec: jobbatch.OneItem(jobbatch.TaskSpec{Kind: "test"}),
		}); err != nil {
			os.Exit(4)
		}
		// The submit commit blocks in the hook; the Runner faults on its own
		// CommitTimeout, the watcher fences, retains, and os.Exit(70) runs.
		// The lock file is never removed or released by any cleanup path.
		select {
		case <-runner.Faulted():
		case <-time.After(10 * time.Second):
			os.Exit(5)
		}
		select {} // wait for the watcher's exit; keeps the helper alive
	}

	t.Parallel()
	exe, err := os.Executable()
	require.NoError(t, err)
	lockPath := filepath.Join(t.TempDir(), "host.lock")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := platform.Command(ctx, exe, "-test.run=^TestAppBatchFaultSubprocess$")
	cmd.Env = append(os.Environ(),
		"R11E_FAULT_HELPER=1",
		"R11E_LOCK_PATH="+lockPath,
	)
	start := time.Now()
	output, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	require.Error(t, err, "the helper must exit non-zero")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", output)
	require.Equal(t, BatchFaultExitCode, exitErr.ExitCode(),
		"never-returning commit hook must exit the process with 70; output: %s", output)
	require.Less(t, elapsed, 30*time.Second,
		"exit must happen within the containment bound, not hang")
	_, statErr := os.Stat(lockPath)
	require.NoError(t, statErr, "the lock file must be retained (never released or removed)")
	require.FileExists(t, lockPath)
}
