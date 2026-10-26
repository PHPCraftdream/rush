package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func batchShellEnv(t *testing.T) batchLeafEnv {
	env := testEnv(t)
	tool := tools.NewBashTool(env.permissions, env.workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, shell.NewBackgroundShellManager())
	registry := newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves
	return batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { return registry, nil }}
}

func batchShellStart(t *testing.T, ctx context.Context, env batchLeafEnv, command string) (*batchShellExecution, <-chan jobbatch.Result) {
	t.Helper()
	input, err := json.Marshal(tools.BashParams{Command: command})
	require.NoError(t, err)
	results := make(chan jobbatch.Result, 2)
	e, err := (batchShellFamily{}).start(ctx, env, jobbatch.Launch{}, batchLeafPayload{Tool: "bash", SessionID: "shell-test", ToolCallID: "c", Input: input}, func(r jobbatch.Result) { results <- r })
	require.NoError(t, err)
	return e.(*batchShellExecution), results
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
	require.Nil(t, e.handle)
	require.NoError(t, e.Dispose(t.Context()))
}

func TestBatchShellStartContextIndependentAndStopDetails(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	e, ch := batchShellStart(t, ctx, batchShellEnv(t), "printf ready; sleep 30")
	cancel()
	readCtx, readCancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer readCancel()
	select {
	case <-e.published:
	case <-readCtx.Done():
		t.Fatal("no retained handle")
	}
	c, err := e.ReadOutput(readCtx, 0)
	require.NoError(t, err)
	require.Equal(t, "ready", string(c.Data))
	require.NoError(t, e.Stop(readCtx))
	require.NoError(t, e.Stop(readCtx))
	r := batchShellResult(t, ch)
	require.Equal(t, jobbatch.StateCancelled, r.State)
	var resp fantasy.ToolResponse
	require.NoError(t, json.Unmarshal(r.Details, &resp))
	require.Contains(t, resp.Content, "ready")
	require.NoError(t, e.Dispose(readCtx))
}

type batchShellHeld struct {
	done      chan struct{}
	observed  chan struct{}
	output    shell.ExecutionOutput
	disposals int
	stopErr   error
	entered   chan struct{}
	release   chan struct{}
}

func (h *batchShellHeld) Done() <-chan struct{} {
	if h.observed != nil {
		close(h.observed)
	}
	return h.done
}

func (h *batchShellHeld) Stop(context.Context) error {
	if h.entered != nil {
		close(h.entered)
		<-h.release
	}
	return h.stopErr
}
func (h *batchShellHeld) GetOutput() (string, string, bool, error) { return "", "", false, nil }
func (h *batchShellHeld) ReadOutput(_ context.Context, cursor uint64) (shell.ExecutionOutput, error) {
	c := h.output
	if cursor > c.Next {
		cursor = c.Next
	}
	start := cursor
	if start < c.Oldest {
		start = c.Oldest
	}
	offset := start - c.Oldest
	if offset > uint64(len(c.Data)) {
		offset = uint64(len(c.Data))
	}
	c.Data = append([]byte(nil), c.Data[offset:]...)
	c.Truncated = c.Truncated || cursor < c.Oldest
	return shell.ExecutionOutput{Data: c.Data, Oldest: c.Oldest, Next: c.Next, EOF: c.EOF, Truncated: c.Truncated}, nil
}
func (h *batchShellHeld) Dispose(context.Context) error { h.disposals++; return nil }

func TestBatchShellStopFeedbackDoesNotSettle(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("physical refused"), context.DeadlineExceeded} {
		h := &batchShellHeld{done: make(chan struct{}), stopErr: stopErr}
		ctx, cancel := context.WithCancel(t.Context())
		e := &batchShellExecution{handle: h, cancel: cancel, finished: make(chan struct{})}
		require.ErrorIs(t, e.Stop(t.Context()), stopErr)
		require.ErrorIs(t, e.Dispose(t.Context()), shell.ErrExecutionNotDone)
		select {
		case <-e.finished:
			t.Fatal("Stop settled")
		default:
		}
		leaf := fantasy.NewAgentTool("bash", "", func(context.Context, tools.BashParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("held"), nil
		})
		reports := make(chan jobbatch.Result, 2)
		go e.run(ctx, leaf, fantasy.ToolCall{Input: "{}"}, func(r jobbatch.Result) { reports <- r })
		close(h.done)
		require.Equal(t, jobbatch.StateCancelled, batchShellResult(t, reports).State)
		require.NoError(t, e.Dispose(t.Context()))
		select {
		case <-reports:
			t.Fatal("duplicate report")
		default:
		}
	}
}

func TestBatchShellReportRequiresRunAndDone(t *testing.T) {
	for _, holdRun := range []bool{false, true} {
		t.Run(fmt.Sprint(holdRun), func(t *testing.T) {
			runEntered, runRelease := make(chan struct{}), make(chan struct{})
			h := &batchShellHeld{done: make(chan struct{}), observed: make(chan struct{})}
			if holdRun {
				close(h.done)
			} else {
				close(runRelease)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e := &batchShellExecution{handle: h, cancel: cancel, finished: make(chan struct{})}
			leaf := fantasy.NewAgentTool("bash", "", func(context.Context, tools.BashParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				close(runEntered)
				<-runRelease
				return fantasy.NewTextResponse("gate"), nil
			})
			reports := make(chan jobbatch.Result, 1)
			go e.run(ctx, leaf, fantasy.ToolCall{Input: "{}"}, func(r jobbatch.Result) { reports <- r })
			<-runEntered
			if !holdRun {
				select {
				case <-h.observed:
				case <-time.After(time.Second):
					t.Error("Run did not reach completion source")
				}
			}
			select {
			case <-reports:
				t.Error("REPORT_BEFORE_BOTH_GATES")
			case <-time.After(50 * time.Millisecond):
			}
			if holdRun {
				close(runRelease)
			} else {
				close(h.done)
			}
			batchShellResult(t, reports)
			require.NoError(t, e.Dispose(t.Context()))
			require.NoError(t, e.Dispose(t.Context()))
			require.Equal(t, 1, h.disposals)
		})
	}
}

func TestBatchShellEarlyStopAndStreamFields(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	e := &batchShellExecution{cancel: cancel, finished: make(chan struct{})}
	require.NoError(t, e.Stop(t.Context()))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.ErrorIs(t, e.Dispose(t.Context()), shell.ErrExecutionNotDone)
	e.handle = &batchShellHeld{output: shell.ExecutionOutput{Data: []byte("tail"), Oldest: 12, Next: 16, Truncated: true, EOF: false}}
	c, err := e.ReadOutput(t.Context(), 0)
	require.NoError(t, err)
	require.EqualValues(t, 12, c.Oldest)
	require.EqualValues(t, 16, c.Next)
	require.True(t, c.Truncated)
	require.False(t, c.EOF)
	require.Equal(t, "tail", string(c.Data))
}

func TestBatchShellDisposeWaitsPhysicalCall(t *testing.T) {
	h := &batchShellHeld{entered: make(chan struct{}), release: make(chan struct{})}
	_, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &batchShellExecution{handle: h, cancel: cancel, finished: make(chan struct{})}
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(t.Context()) }()
	<-h.entered
	close(e.finished)
	disposing := make(chan error, 1)
	go func() { disposing <- e.Dispose(t.Context()) }()
	select {
	case <-disposing:
		t.Error("DISPOSE_PASSED_ACTIVE_STOP")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.release)
	require.NoError(t, <-stopped)
	require.NoError(t, <-disposing)
	require.Equal(t, 1, h.disposals)
}

func TestBatchShellEarlyStopPolicyGateStartsNoProcess(t *testing.T) {
	env := testEnv(t)
	manager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { manager.Close(context.Background()) })
	underlying := tools.NewBashTool(env.permissions, env.workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test", nil, manager)
	entered, release := make(chan struct{}), make(chan struct{})
	leaf := fantasy.NewAgentTool("bash", "", func(ctx context.Context, p tools.BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		close(entered)
		<-release
		return underlying.Run(ctx, call)
	})
	registry := newPolicyToolBuild([]fantasy.AgentTool{leaf}).leaves
	e, reports := batchShellStart(t, t.Context(), batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { return registry, nil }}, "echo must-not-spawn")
	<-entered
	require.NoError(t, e.Stop(t.Context()))
	close(release)
	require.Equal(t, jobbatch.StateCancelled, batchShellResult(t, reports).State)
	require.Nil(t, e.handle, "cancelled setup must not publish a process")
	require.Empty(t, manager.List())
	require.NoError(t, e.Dispose(t.Context()))
}
