package batchexec

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

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

type batchShellHeld struct {
	done      chan struct{}
	observed  chan struct{}
	output    shellbg.ExecutionOutput
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
func (h *batchShellHeld) ReadOutput(_ context.Context, cursor uint64) (shellbg.ExecutionOutput, error) {
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
	return shellbg.ExecutionOutput{Data: c.Data, Oldest: c.Oldest, Next: c.Next, EOF: c.EOF, Truncated: c.Truncated}, nil
}
func (h *batchShellHeld) Dispose(context.Context) error { h.disposals++; return nil }

func TestBatchShellStopFeedbackDoesNotSettle(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("physical refused"), context.DeadlineExceeded} {
		h := &batchShellHeld{done: make(chan struct{}), stopErr: stopErr}
		ctx, cancel := context.WithCancel(t.Context())
		e := NewShell(cancel, Output{Result: batchdispatch.ResultFromResponse}, nil)
		e.Publish(h)
		require.ErrorIs(t, e.Stop(t.Context()), stopErr)
		require.ErrorIs(t, e.Dispose(t.Context()), shellbg.ErrExecutionNotDone)
		select {
		case <-e.finished:
			t.Fatal("Stop settled")
		default:
		}
		leaf := fantasy.NewAgentTool("bash", "", func(context.Context, tools.BashParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("held"), nil
		})
		reports := make(chan jobbatch.Result, 2)
		go e.Run(ctx, leaf, fantasy.ToolCall{Input: "{}"}, func(r jobbatch.Result) { reports <- r })
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
			e := NewShell(cancel, Output{Result: batchdispatch.ResultFromResponse}, nil)
			e.Publish(h)
			leaf := fantasy.NewAgentTool("bash", "", func(context.Context, tools.BashParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				close(runEntered)
				<-runRelease
				return fantasy.NewTextResponse("gate"), nil
			})
			reports := make(chan jobbatch.Result, 1)
			go e.Run(ctx, leaf, fantasy.ToolCall{Input: "{}"}, func(r jobbatch.Result) { reports <- r })
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
	e := NewShell(cancel, Output{Result: batchdispatch.ResultFromResponse}, nil)
	require.NoError(t, e.Stop(t.Context()))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.ErrorIs(t, e.Dispose(t.Context()), shellbg.ErrExecutionNotDone)
	require.NotNil(t, e.published)
	select {
	case <-e.published:
		t.Fatal("publication signaled before Publish")
	default:
	}
	_, err := e.ReadOutput(t.Context(), 0)
	require.ErrorIs(t, err, jobbatch.ErrNotStarted)

	h := &batchShellHeld{output: shellbg.ExecutionOutput{Data: []byte("tail"), Oldest: 12, Next: 16, Truncated: true, EOF: false}}
	e.Publish(h)
	select {
	case _, ok := <-e.published:
		require.False(t, ok, "publication channel must be closed")
	default:
		t.Fatal("publication not signaled after Publish")
	}
	require.Same(t, h, e.handle)

	c, err := e.ReadOutput(t.Context(), 0)
	require.NoError(t, err)
	require.EqualValues(t, 12, c.Oldest)
	require.EqualValues(t, 16, c.Next)
	require.True(t, c.Truncated)
	require.False(t, c.EOF)
	require.Equal(t, "tail", string(c.Data))
}
