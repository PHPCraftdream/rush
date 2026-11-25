package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

func batchShellEnv(t *testing.T) batchLeafEnv {
	env := testEnv(t)
	tool := tools.NewBashTool(env.permissions, env.workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shellbg.NewBackgroundShellManager())
	registry := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{tool}).Leaves
	return batchLeafEnv{Leaves: func(string) (*toolwrap.PolicyLeafRegistry, error) { return registry, nil }}
}

func batchShellStart(t *testing.T, ctx context.Context, env batchLeafEnv, command string) (*batchexec.ShellExecution, <-chan jobbatch.Result) {
	t.Helper()
	input, err := json.Marshal(tools.BashParams{Command: command})
	require.NoError(t, err)
	results := make(chan jobbatch.Result, 2)
	e, err := (batchShellFamily{}).start(ctx, env, jobbatch.Launch{}, batchLeafPayload{Tool: "bash", SessionID: "shell-test", ToolCallID: "c", Input: input}, func(r jobbatch.Result) { results <- r })
	require.NoError(t, err)
	return e.(*batchexec.ShellExecution), results
}

func batchShellResult(t *testing.T, ch <-chan jobbatch.Result) jobbatch.Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("no shell report")
		return jobbatch.Result{}
	}
}

func TestBatchShellRealSuccessExitAndDetails(t *testing.T) {
	for _, tc := range []struct{ cmd, code string }{{"echo success", ""}, {"printf failure; exit 7", "exit_7"}} {
		e, ch := batchShellStart(t, t.Context(), batchShellEnv(t), tc.cmd)
		r := batchShellResult(t, ch)
		require.Equal(t, tc.code, r.Code)
		if tc.code == "" {
			require.Equal(t, jobbatch.StateCompleted, r.State)
		} else {
			require.Equal(t, jobbatch.StateFailed, r.State)
		}
		var resp fantasy.ToolResponse
		require.NoError(t, json.Unmarshal(r.Details, &resp))
		require.NotEmpty(t, resp.Content)
		c, err := e.ReadOutput(t.Context(), 0)
		require.NoError(t, err)
		require.NotEmpty(t, c.Data)
		require.True(t, c.EOF)
		require.NoError(t, e.Dispose(t.Context()))
		_, err = e.ReadOutput(t.Context(), 0)
		require.ErrorIs(t, err, jobbatch.ErrReleased)
	}
}

func TestBatchShellRejectedAndDenied(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (batchShellFamily{}).start(ctx, batchLeafEnv{}, jobbatch.Launch{}, batchLeafPayload{Tool: "bash"}, nil)
	require.ErrorIs(t, err, context.Canceled)
	_, err = (batchShellFamily{}).start(t.Context(), batchLeafEnv{}, jobbatch.Launch{}, batchLeafPayload{Tool: "view"}, nil)
	require.Error(t, err)
	e, ch := batchShellStart(t, t.Context(), batchShellEnv(t), "sudo echo denied")
	r := batchShellResult(t, ch)
	require.Equal(t, jobbatch.StateFailed, r.State)
	_, err = e.ReadOutput(t.Context(), 0)
	require.ErrorIs(t, err, jobbatch.ErrNotStarted)
	require.NoError(t, e.Dispose(t.Context()))
}

func TestBatchShellStartContextIndependentAndStopDetails(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	e, ch := batchShellStart(t, ctx, batchShellEnv(t), "printf ready; sleep 30")
	cancel()
	readCtx, readCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer readCancel()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	var output []byte
	var cursor uint64
	for {
		c, err := e.ReadOutput(readCtx, cursor)
		if !errors.Is(err, jobbatch.ErrNotStarted) {
			require.NoError(t, err)
			output = append(output, c.Data...)
			cursor = c.Next
			require.False(t, c.EOF, "shell exited before Stop")
			if len(output) >= len("ready") {
				require.Equal(t, "ready", string(output))
				break
			}
		}
		select {
		case <-readCtx.Done():
			t.Fatalf("waiting for shell output: %v (got %q)", readCtx.Err(), output)
		case <-retry.C:
		}
	}
	require.NoError(t, e.Stop(readCtx))
	require.NoError(t, e.Stop(readCtx))
	r := batchShellResult(t, ch)
	require.Equal(t, jobbatch.StateCancelled, r.State)
	var resp fantasy.ToolResponse
	require.NoError(t, json.Unmarshal(r.Details, &resp))
	require.Contains(t, resp.Content, "ready")
	require.NoError(t, e.Dispose(readCtx))
}

func TestBatchShellEarlyStopPolicyGateStartsNoProcess(t *testing.T) {
	env := testEnv(t)
	manager := shellbg.NewBackgroundShellManager()
	t.Cleanup(func() { manager.Close(context.Background()) })
	underlying := tools.NewBashTool(env.permissions, env.workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, manager)
	entered, release := make(chan struct{}), make(chan struct{})
	leaf := fantasy.NewAgentTool("bash", "", func(ctx context.Context, p tools.BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(entered)
		<-release
		return underlying.Run(ctx, call)
	})
	registry := toolwrap.NewPolicyToolBuild([]fantasy.AgentTool{leaf}).Leaves
	e, reports := batchShellStart(t, t.Context(), batchLeafEnv{Leaves: func(string) (*toolwrap.PolicyLeafRegistry, error) { return registry, nil }}, "echo must-not-spawn")
	<-entered
	require.NoError(t, e.Stop(t.Context()))
	close(release)
	require.Equal(t, jobbatch.StateCancelled, batchShellResult(t, reports).State)
	_, err := e.ReadOutput(t.Context(), 0)
	require.ErrorIs(t, err, jobbatch.ErrNotStarted, "cancelled setup must not publish a process")
	require.Empty(t, manager.List())
	require.NoError(t, e.Dispose(t.Context()))
}

// Compatibility fixture for batch_terminal_handoff_test.go.
type batchShellExecution struct {
	*batchexec.ShellExecution
	cache          *batchOutputCache
	nodeID, rootID jobbatch.ID
	handle         batchexec.ShellHandle
	cancel         context.CancelFunc
	finished       chan struct{}
}

func (e *batchShellExecution) run(ctx context.Context, tool fantasy.AgentTool, call fantasy.ToolCall, report func(jobbatch.Result)) {
	e.ShellExecution = newRetainedShell(e.cancel, e.cache, e.nodeID, e.rootID)
	if e.handle != nil {
		e.Publish(e.handle)
	}
	e.Run(ctx, tool, call, report)
}
