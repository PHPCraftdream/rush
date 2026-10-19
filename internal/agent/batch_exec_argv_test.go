package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/procinfo"
	"github.com/stretchr/testify/require"
)

// Argv-selected helper, without environment overrides or TestMain changes.
func TestBatchArgvHelper(t *testing.T) {
	idx := -1
	for i, arg := range os.Args {
		if arg == "--batch-argv-helper" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	mode := os.Args[idx+1]
	switch mode {
	case "output":
		fmt.Print("hello argv\n")
	case "exit":
		fmt.Print("failed argv\n")
		os.Exit(7)
	case "bulk":
		fmt.Print(strings.Repeat("x", (1<<20)+123))
	case "tree", "hold":
		if mode == "tree" {
			self, err := os.Executable()
			if err != nil {
				os.Exit(2)
			}
			cmd := platform.Command(context.Background(), self, "-test.run=^TestBatchArgvHelper$", "--", "--batch-argv-helper", "hold")
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if cmd.Start() != nil {
				os.Exit(2)
			}
			fmt.Printf("parent=%d\n", os.Getpid())
		} else {
			fmt.Printf("child=%d\n", os.Getpid())
		}
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

type batchArgvPerms struct {
	permission.Service
	request func(context.Context, permission.CreatePermissionRequest) (bool, error)
}

func (p batchArgvPerms) Request(ctx context.Context, r permission.CreatePermissionRequest) (bool, error) {
	return p.request(ctx, r)
}

func batchArgvTool(t *testing.T, allow bool) fantasy.AgentTool {
	t.Helper()
	return tools.NewRunCommandTool(batchArgvPerms{request: func(context.Context, permission.CreatePermissionRequest) (bool, error) { return allow, nil }}, t.TempDir())
}

func batchArgvPayload(t *testing.T, mode string) batchLeafPayload {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	raw, err := json.Marshal(tools.RunCommandParams{Program: self, Args: []string{"-test.run=^TestBatchArgvHelper$", "--", "--batch-argv-helper", mode}, TimeoutSeconds: 30})
	require.NoError(t, err)
	return batchLeafPayload{Tool: tools.RunCommandToolName, SessionID: "argv-session", ToolCallID: "argv-call", Input: raw}
}

func batchArgvEnv(tool fantasy.AgentTool) batchLeafEnv {
	return batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) {
		return newPolicyToolBuild([]fantasy.AgentTool{tool}).leaves, nil
	}}
}

func batchArgvContext(t *testing.T) context.Context {
	t.Helper()
	return context.WithValue(t.Context(), tools.SessionIDContextKey, "argv-session")
}

func batchArgvStart(t *testing.T, ctx context.Context, tool fantasy.AgentTool, p batchLeafPayload) (*batchArgvExecution, <-chan jobbatch.Result) {
	t.Helper()
	reports := make(chan jobbatch.Result, 2)
	ex, err := (batchArgvFamily{}).start(ctx, batchArgvEnv(tool), jobbatch.Launch{}, p, func(r jobbatch.Result) { reports <- r })
	require.NoError(t, err)
	e := ex.(*batchArgvExecution)
	t.Cleanup(func() {
		_ = e.Stop(context.Background())
		select {
		case <-e.finished:
		case <-time.After(20 * time.Second):
			t.Error("execution did not drain")
		}
	})
	_, messenger := ex.(jobbatch.Messenger)
	require.False(t, messenger)
	return e, reports
}

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

func TestBatchArgvResponseAndOutput(t *testing.T) {
	for _, mode := range []string{"output", "exit", "bulk"} {
		t.Run(mode, func(t *testing.T) {
			tool := batchArgvTool(t, true)
			p := batchArgvPayload(t, mode)
			normal, err := tool.Run(batchArgvContext(t), fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)})
			require.NoError(t, err)
			e, reports := batchArgvStart(t, batchArgvContext(t), tool, p)
			r := batchArgvReport(t, reports)
			batchArgvNoReport(t, reports)
			var back fantasy.ToolResponse
			require.NoError(t, json.Unmarshal(r.Details, &back))
			require.Equal(t, normal.Content, back.Content)
			require.Equal(t, normal.IsError, back.IsError)
			var a, b tools.RunCommandResponseMetadata
			require.NoError(t, json.Unmarshal([]byte(normal.Metadata), &a))
			require.NoError(t, json.Unmarshal([]byte(back.Metadata), &b))
			a.StartTime, a.EndTime, b.StartTime, b.EndTime = 0, 0, 0, 0
			require.Equal(t, a, b)
			if mode == "exit" {
				require.Equal(t, jobbatch.StateFailed, r.State)
				require.Equal(t, "exit_7", r.Code)
				require.True(t, jobbatch.ValidCode(r.Code))
				require.False(t, jobbatch.ReservedCode(r.Code))
			} else {
				require.Equal(t, jobbatch.StateCompleted, r.State)
			}
			for _, cursor := range []uint64{0, math.MaxUint64} {
				c, err := e.ReadOutput(t.Context(), cursor)
				require.NoError(t, err)
				require.True(t, c.EOF)
				if mode == "bulk" {
					require.True(t, c.Truncated)
					require.EqualValues(t, 123, c.Oldest)
					require.EqualValues(t, (1<<20)+123, c.Next)
					require.Len(t, c.Data, 1<<20)
				} else {
					require.Contains(t, string(c.Data), "argv")
				}
				end, err := e.ReadOutput(t.Context(), c.Next)
				require.NoError(t, err)
				require.Empty(t, end.Data)
				require.True(t, end.EOF)
			}
			require.NoError(t, e.Stop(t.Context()))
			require.NoError(t, e.Dispose(t.Context()))
			require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvDisposed)
			_, err = e.ReadOutput(t.Context(), 0)
			require.ErrorIs(t, err, tools.ErrArgvDisposed)
		})
	}
}

func TestBatchArgvCancelledStartAndValidation(t *testing.T) {
	var resolutions atomic.Int32
	env := batchLeafEnv{Leaves: func(string) (*policyLeafRegistry, error) { resolutions.Add(1); return nil, errors.New("missing") }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ex, err := (batchArgvFamily{}).start(ctx, env, jobbatch.Launch{}, batchArgvPayload(t, "output"), func(jobbatch.Result) { t.Error("report on rejected Start") })
	require.Nil(t, ex)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, resolutions.Load())
	p := batchArgvPayload(t, "output")
	p.Tool = "bash"
	_, err = (batchArgvFamily{}).start(t.Context(), env, jobbatch.Launch{}, p, nil)
	require.Error(t, err)
	require.Zero(t, resolutions.Load())
	p.Tool = tools.RunCommandToolName
	_, err = (batchArgvFamily{}).start(t.Context(), env, jobbatch.Launch{}, p, nil)
	require.Error(t, err)
	wrong := fantasy.NewAgentTool("wrong", "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		t.Error("wrong leaf ran")
		return fantasy.ToolResponse{}, nil
	})
	env.Leaves = func(string) (*policyLeafRegistry, error) {
		return &policyLeafRegistry{entries: map[string]policyLeafEntry{tools.RunCommandToolName: {tool: wrong}}}, nil
	}
	_, err = (batchArgvFamily{}).start(t.Context(), env, jobbatch.Launch{}, p, nil)
	require.Error(t, err)
}

func TestBatchArgvDeniedGuardAndSetupStop(t *testing.T) {
	for _, mode := range []string{"denied", "guard", "setup-stop"} {
		t.Run(mode, func(t *testing.T) {
			p := batchArgvPayload(t, "output")
			tool := batchArgvTool(t, false)
			entered, release := make(chan struct{}), make(chan struct{})
			if mode == "guard" {
				p.Input = json.RawMessage(`{"program":"curl"}`)
			}
			if mode == "setup-stop" {
				tool = tools.NewRunCommandTool(batchArgvPerms{request: func(ctx context.Context, _ permission.CreatePermissionRequest) (bool, error) {
					close(entered)
					<-release
					return false, ctx.Err()
				}}, t.TempDir())
			}
			e, reports := batchArgvStart(t, batchArgvContext(t), tool, p)
			if mode == "setup-stop" {
				<-entered
				require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
				require.NoError(t, e.Stop(t.Context()))
				batchArgvNoReport(t, reports)
				close(release)
			}
			r := batchArgvReport(t, reports)
			if mode == "setup-stop" {
				require.Equal(t, jobbatch.StateCancelled, r.State)
			} else {
				require.Equal(t, jobbatch.StateFailed, r.State)
			}
			c, err := e.ReadOutput(t.Context(), math.MaxUint64)
			require.NoError(t, err)
			require.True(t, c.EOF)
			require.Empty(t, c.Data)
			require.Zero(t, c.Next)
			require.NoError(t, e.Dispose(t.Context()))
			require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvDisposed)
		})
	}
}

func TestBatchArgvSetupStopRacingSuccessfulSpawn(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	tool := tools.NewRunCommandTool(batchArgvPerms{request: func(context.Context, permission.CreatePermissionRequest) (bool, error) {
		close(entered)
		<-release
		return true, nil
	}}, t.TempDir())
	e, reports := batchArgvStart(t, batchArgvContext(t), tool, batchArgvPayload(t, "hold"))
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

func TestBatchArgvStartContextIndependentAndTreeGone(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	key := struct{ label string }{"retained"}
	tool := tools.NewRunCommandTool(batchArgvPerms{request: func(ctx context.Context, r permission.CreatePermissionRequest) (bool, error) {
		if ctx.Value(key) != "value" || r.ToolCallID != "argv-call" {
			return false, errors.New("lost context or identity")
		}
		close(entered)
		<-release
		return true, ctx.Err()
	}}, t.TempDir())
	ctx, cancel := context.WithCancel(context.WithValue(batchArgvContext(t), key, "value"))
	defer cancel()
	e, reports := batchArgvStart(t, ctx, tool, batchArgvPayload(t, "tree"))
	<-entered
	cancel()
	close(release)
	readCtx, end := context.WithTimeout(t.Context(), 20*time.Second)
	defer end()
	text := ""
	cursor := uint64(0)
	for !strings.Contains(text, "parent=") || !strings.Contains(text, "child=") || !strings.HasSuffix(text, "\n") {
		c, err := e.ReadOutput(readCtx, cursor)
		require.NoError(t, err)
		require.False(t, c.EOF)
		text += string(c.Data)
		cursor = c.Next
	}
	pids := []int{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.Split(line, "=")
		require.Len(t, parts, 2)
		pid, err := strconv.Atoi(parts[1])
		require.NoError(t, err)
		pids = append(pids, pid)
		require.True(t, procinfo.Alive(pid))
	}
	require.Len(t, pids, 2)
	batchArgvNoReport(t, reports)
	require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
	require.NoError(t, e.Stop(readCtx))
	require.NoError(t, e.Stop(readCtx))
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateCancelled, r.State)
	// Windows can prove concrete process death. Unix orphan zombies remain
	// visible to kill(0), so do not mislabel a zombie as a running descendant.
	if runtime.GOOS == "windows" {
		for _, pid := range pids {
			alive, _, known := procinfo.Probe(pid)
			require.True(t, known)
			require.False(t, alive, "process %d alive at report", pid)
		}
	}
	c, err := e.ReadOutput(readCtx, 0)
	require.NoError(t, err)
	require.True(t, c.EOF)
	require.Contains(t, string(c.Data), "child=")
	batchArgvNoReport(t, reports)
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
			e := &batchArgvExecution{process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
			leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("done"), nil
			})
			reports := make(chan jobbatch.Result, 2)
			go e.run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
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

func TestBatchArgvDisposeOneShotLatePhysicalRelease(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{})
	p := &batchArgvControlled{done: make(chan struct{}), dispose: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	close(p.done)
	e := &batchArgvExecution{process: p, finished: make(chan struct{})}
	close(e.finished) // Family has reported Run + Done before its one cleanup call.
	answer := make(chan error, 1)
	go func() { answer <- e.Dispose(t.Context()) }()
	<-entered
	close(release)
	require.NoError(t, <-answer)
	require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvDisposed)
	require.EqualValues(t, 1, p.disposals.Load())
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
			e := &batchArgvExecution{process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
			reports := make(chan jobbatch.Result, 2)
			go e.run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
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
			e := &batchArgvExecution{process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
			leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse("done"), nil
			})
			reports := make(chan jobbatch.Result, 2)
			go e.run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
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
	e := &batchArgvExecution{process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
	resp := fantasy.WithResponseMetadata(fantasy.NewTextResponse("wrapper success"), map[string]string{"retained": "yes"})
	leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) { return resp, nil })
	reports := make(chan jobbatch.Result, 2)
	go e.run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Equal(t, "exit_23", r.Code)
	require.JSONEq(t, string(batchEnvelope(resp)), string(r.Details))
	batchArgvNoReport(t, reports)
}

func TestBatchArgvPanicContainsPublishedProcess(t *testing.T) {
	stopped, done := make(chan struct{}), make(chan struct{})
	p := &batchArgvControlled{done: done, stop: func(context.Context) error { close(stopped); return errors.New("stop not proof") }}
	e := &batchArgvExecution{process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
	leaf := fantasy.NewAgentTool(tools.RunCommandToolName, "", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		panic("wrapper panic")
	})
	reports := make(chan jobbatch.Result, 2)
	go e.run(t.Context(), leaf, fantasy.ToolCall{Input: `{}`}, func(r jobbatch.Result) { reports <- r })
	<-stopped
	batchArgvNoReport(t, reports)
	require.ErrorIs(t, e.Dispose(t.Context()), tools.ErrArgvNotDone)
	close(done)
	r := batchArgvReport(t, reports)
	require.Equal(t, jobbatch.StateFailed, r.State)
	require.Contains(t, r.Summary, "wrapper panic")
	batchArgvNoReport(t, reports)
}
