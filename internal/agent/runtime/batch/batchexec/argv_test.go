package batchexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/stretchr/testify/require"
)

func batchArgvReport(t *testing.T, reports <-chan jobbatch.Result) jobbatch.Result {
	t.Helper()
	select {
	case r := <-reports:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("missing report")
	}
	return jobbatch.Result{}
}

func batchArgvNoReport(t *testing.T, reports <-chan jobbatch.Result) {
	t.Helper()
	select {
	case r := <-reports:
		t.Fatalf("premature/duplicate report: %+v", r)
	default:
	}
}

// Controlled handle isolates Run-return, physical completion and Stop answer.
type batchArgvControlled struct {
	done      chan struct{}
	joined    chan struct{}
	exitCode  int
	stop      func(context.Context) error
	reads     chan int64
	disposals atomic.Int32
	dispose   func(context.Context) error
}

func (p *batchArgvControlled) Done() <-chan struct{} {
	if p.joined != nil {
		close(p.joined)
	}
	return p.done
}

func (p *batchArgvControlled) Outcome() (tools.ArgvOutcome, error) {
	return tools.ArgvOutcome{ExitCode: p.exitCode}, nil
}
func (p *batchArgvControlled) Stop(ctx context.Context) error { return p.stop(ctx) }
func (p *batchArgvControlled) ReadOutput(_ context.Context, c int64) (jobbatch.OutputChunk, error) {
	p.reads <- c
	return jobbatch.OutputChunk{Data: []byte("argv-output"), Next: 11, EOF: true}, nil
}

func (p *batchArgvControlled) Dispose(ctx context.Context) error {
	p.disposals.Add(1)
	if p.dispose != nil {
		return p.dispose(ctx)
	}
	return nil
}

func TestBatchArgvStopLateAckNonzero(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(fmt.Sprintf("requested=%v", requested), func(t *testing.T) {
			ack := make(chan struct{})
			defer close(ack)
			physical := make(chan struct{})
			p := &batchArgvControlled{done: make(chan struct{}), exitCode: 23}
			p.stop = func(ctx context.Context) error {
				// Successful physical kill, but its acknowledgement is held.
				close(physical)
				go func() { <-ack }()
				<-ctx.Done()
				return ctx.Err()
			}
			e := NewArgv(func() {}, Output{Result: batchdispatch.ResultFromResponse}, nil)
			e.Publish(p)
			leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("done"), nil
			})
			reports := make(chan jobbatch.Result, 2)
			go e.Run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
			if requested {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, e.Stop(ctx), context.DeadlineExceeded)
				<-physical
				batchArgvNoReport(t, reports)
				require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
			}
			close(p.done)
			r := batchArgvReport(t, reports)
			if requested {
				require.Equal(t, jobbatch.StateCancelled, r.State)
			} else {
				require.Equal(t, jobbatch.StateFailed, r.State)
				require.Equal(t, "exit_23", r.Code)
			}
			batchArgvNoReport(t, reports)
		})
	}
}

func TestBatchArgvReportJoinsRunAndDone(t *testing.T) {
	for _, first := range []string{"run", "done"} {
		t.Run(first, func(t *testing.T) {
			p := &batchArgvControlled{done: make(chan struct{}), joined: make(chan struct{}), stop: func(context.Context) error { return nil }}
			entered, release := make(chan struct{}), make(chan struct{})
			leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				close(entered)
				<-release
				return fantasy.NewImageResponse([]byte{1, 2, 3}, "image/png"), nil
			})
			e := NewArgv(func() {}, Output{Result: batchdispatch.ResultFromResponse}, nil)
			e.Publish(p)
			reports := make(chan jobbatch.Result, 2)
			go e.Run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
			t.Cleanup(func() {
				for _, ch := range []chan struct{}{release, p.done} {
					select {
					case <-ch:
					default:
						close(ch)
					}
				}
			})
			<-entered
			if first == "run" {
				close(release)
				select {
				case <-p.joined: // Run returned; report must wait for real Done.
				case r := <-reports:
					t.Fatalf("reported before real Done join: %+v", r)
				case <-time.After(5 * time.Second):
					t.Fatal("Run return did not join real Done or report")
				}
			} else {
				close(p.done)
			}
			require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
			batchArgvNoReport(t, reports)
			if first == "run" {
				close(p.done)
			} else {
				close(release)
			}
			r := batchArgvReport(t, reports)
			require.Equal(t, jobbatch.StateCompleted, r.State)
			var resp fantasy.ToolResponse
			require.NoError(t, json.Unmarshal(r.Details, &resp))
			require.Equal(t, []byte{1, 2, 3}, resp.Data)
			require.NoError(t, e.Dispose(t.Context()))
			require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvDisposed)
			require.EqualValues(t, 1, p.disposals.Load())
			batchArgvNoReport(t, reports)
		})
	}
}

func TestBatchArgvStopAnswersNeverSettle(t *testing.T) {
	for _, mode := range []string{"ack", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			sentinel := errors.New("kill failed")
			calls := 0
			entered := make(chan struct{}, 2)
			p := &batchArgvControlled{done: make(chan struct{}), reads: make(chan int64, 2), stop: func(ctx context.Context) error {
				calls++
				entered <- struct{}{}
				switch mode {
				case "error":
					return sentinel
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}}
			e := NewArgv(func() {}, Output{Result: batchdispatch.ResultFromResponse}, nil)
			e.Publish(p)
			leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("done"), nil
			})
			reports := make(chan jobbatch.Result, 2)
			go e.Run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithCancel(t.Context())
				answer := make(chan error, 1)
				go func() { answer <- e.Stop(ctx) }()
				<-entered
				if mode == "timeout" {
					cancel()
				}
				err := <-answer
				cancel()
				switch mode {
				case "ack":
					require.NoError(t, err)
				case "error":
					require.ErrorIs(t, err, sentinel)
				case "timeout":
					require.ErrorIs(t, err, context.Canceled)
				}
				batchArgvNoReport(t, reports)
				require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
			}
			require.Equal(t, 2, calls)
			_, err := e.ReadOutput(t.Context(), math.MaxUint64)
			require.NoError(t, err)
			require.Zero(t, <-p.reads)
			close(p.done)
			r := batchArgvReport(t, reports)
			require.Equal(t, jobbatch.StateCancelled, r.State)
			batchArgvNoReport(t, reports)
		})
	}
}

func TestBatchArgvOutcomeOverridesSuccessfulEnvelope(t *testing.T) {
	p := &batchArgvControlled{done: make(chan struct{}), exitCode: 23}
	close(p.done)
	e := NewArgv(func() {}, Output{Result: batchdispatch.ResultFromResponse}, nil)
	e.Publish(p)
	resp := fantasy.WithResponseMetadata(fantasy.NewTextResponse("wrapper success"), map[string]string{"retained": "yes"})
	leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) { return resp, nil })
	reports := make(chan jobbatch.Result, 2)
	go e.Run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Equal(t, "exit_23", r.Code)
	require.JSONEq(t, string(batchdispatch.Envelope(resp)), string(r.Details))
	batchArgvNoReport(t, reports)
}

func TestBatchArgvPanicContainsPublishedProcess(t *testing.T) {
	stopped, done := make(chan struct{}), make(chan struct{})
	p := &batchArgvControlled{done: done, stop: func(context.Context) error { close(stopped); return errors.New("stop not proof") }}
	e := NewArgv(func() {}, Output{Result: batchdispatch.ResultFromResponse}, nil)
	e.Publish(p)
	leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		panic("wrapper panic")
	})
	reports := make(chan jobbatch.Result, 2)
	go e.Run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
	<-stopped
	batchArgvNoReport(t, reports)
	require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
	close(done)
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Contains(t, r.Summary, "wrapper panic")
	batchArgvNoReport(t, reports)
}

// TestBatchArgvHelper is selected only by the retained-process race fixture.
func TestBatchArgvHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--batch-argv-helper" && i+1 < len(os.Args) && os.Args[i+1] == "hold" {
			fmt.Printf("child=%d\n", os.Getpid())
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, os.Interrupt)
			<-ch
			os.Exit(0)
		}
	}
}

type batchArgvPerms struct {
	permission.Service
	request func(context.Context, permission.CreatePermissionRequest) (bool, error)
}

func (p batchArgvPerms) Request(ctx context.Context, r permission.CreatePermissionRequest) (bool, error) {
	return p.request(ctx, r)
}

func TestBatchArgvSetupStopRacingSuccessfulSpawn(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	tool := tools.NewRunCommandTool(batchArgvPerms{request: func(context.Context, permission.CreatePermissionRequest) (bool, error) {
		close(entered)
		<-release
		return true, nil
	}}, t.TempDir())
	self, err := os.Executable()
	require.NoError(t, err)
	input, err := json.Marshal(tools.RunCommandParams{Program: self, Args: []string{"-test.run=^TestBatchArgvHelper$", "--", "--batch-argv-helper", "hold"}, TimeoutSeconds: 30})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "argv-session")
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	e := NewArgv(cancel, Output{Result: batchdispatch.ResultFromResponse}, nil)
	reports := make(chan jobbatch.Result, 2)
	go e.Run(e.WithArgvHandle(runCtx), tool, fantasy.ToolCall{ID: "argv-call", Name: tools.RunCommandToolName, Input: string(input)}, func(r jobbatch.Result) { reports <- r })
	t.Cleanup(func() {
		_ = e.Stop(context.Background())
		select {
		case <-e.finished:
		case <-time.After(20 * time.Second):
			t.Error("execution did not drain")
		}
	})
	<-entered
	require.NoError(t, e.Stop(t.Context()))
	batchArgvNoReport(t, reports)
	close(release)
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateCancelled, r.State)
	e.mu.Lock()
	p := e.process
	e.mu.Unlock()
	require.NotNil(t, p)
	select {
	case <-p.Done():
	default:
		t.Fatal("reported with spawned process alive")
	}
	require.NoError(t, e.Dispose(t.Context()))
	batchArgvNoReport(t, reports)
}
