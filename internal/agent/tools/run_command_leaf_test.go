package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

const argvHelper = "__argv_leaf_helper__"

func runArgvLeafHelper() bool {
	if len(os.Args) < 3 || os.Args[1] != argvHelper {
		return false
	}
	switch os.Args[2] {
	case "output":
		fmt.Print("hello stdout\n")
		fmt.Fprint(os.Stderr, "hello stderr\n")
	case "exit":
		fmt.Print("failed\n")
		os.Exit(7)
	case "empty":
	case "tree":
		self, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		cmd := platform.Command(context.Background(), self, argvHelper, "hold")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if cmd.Start() != nil {
			os.Exit(2)
		}
		fmt.Println("parent-ready")
		argvHelperHold()
	case "hold":
		fmt.Println("child-ready")
		argvHelperHold()
	default:
		os.Exit(2)
	}
	return true
}

func argvHelperHold() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
}

func argvParams(t *testing.T, mode string) RunCommandParams {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	return RunCommandParams{Program: self, Args: []string{argvHelper, mode}}
}

func argvContext() context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, "argv-session")
}

func TestRunCommandLeafEquivalence(t *testing.T) {
	for _, mode := range []string{"output", "exit", "empty"} {
		t.Run(mode, func(t *testing.T) {
			tool := newRunCommandToolForTest(t, t.TempDir())
			params := argvParams(t, mode)
			normal := runRunCommandTool(t, tool, argvContext(), params)
			var process *ArgvProcess
			leaf := runRunCommandTool(t, tool, WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(p *ArgvProcess) {
				process = p
				select {
				case <-p.Done():
					t.Error("Wait preceded OnStart")
				default:
				}
				require.ErrorIs(t, p.Dispose(context.Background()), ErrArgvNotDone)
			}}), params)
			require.NotNil(t, process)
			require.Equal(t, normal.Content, leaf.Content)
			require.Equal(t, normal.IsError, leaf.IsError)
			var a, b RunCommandResponseMetadata
			require.NoError(t, json.Unmarshal([]byte(normal.Metadata), &a))
			require.NoError(t, json.Unmarshal([]byte(leaf.Metadata), &b))
			a.StartTime, a.EndTime, b.StartTime, b.EndTime = 0, 0, 0, 0
			require.Equal(t, a, b)
			outcome, err := process.Outcome()
			require.NoError(t, err)
			if mode == "exit" {
				require.Equal(t, 7, outcome.ExitCode)
				require.Error(t, outcome.WaitErr)
			} else {
				require.Zero(t, outcome.ExitCode)
				require.NoError(t, outcome.WaitErr)
			}
			chunk, err := process.ReadOutput(t.Context(), 0)
			require.NoError(t, err)
			require.True(t, chunk.EOF)
			if mode == "output" {
				require.Contains(t, string(chunk.Data), "hello stdout")
				require.Contains(t, string(chunk.Data), "hello stderr")
			}
			require.NoError(t, process.Stop(t.Context()))
			require.NoError(t, process.Dispose(t.Context()))
			require.ErrorIs(t, process.Dispose(t.Context()), ErrArgvDisposed)
			_, err = process.ReadOutput(t.Context(), 0)
			require.ErrorIs(t, err, ErrArgvDisposed)
		})
	}
}

func TestRunCommandLeafGuardsAndPermission(t *testing.T) {
	for _, params := range []RunCommandParams{
		{},
		{Program: "curl"},
		{Program: "bash", Args: []string{"-c", "claude"}},
		{Program: "go", Args: []string{"version"}, WorkingDir: "../escape"},
		{Program: "git", Args: []string{"checkout", "x"}},
		{Program: "go", TimeoutSeconds: -1},
	} {
		tool, perms := newRunCommandToolWithRecordingPerms(t, linkedWorktreeFixture(t))
		normal := runRunCommandTool(t, tool, argvContext(), params)
		require.True(t, normal.IsError)
		require.Zero(t, perms.requestCount)
		called := false
		resp := runRunCommandTool(t, tool, WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(*ArgvProcess) { called = true }}), params)
		require.Equal(t, normal, resp, "leaf must preserve the identical normal guard refusal")
		require.True(t, resp.IsError)
		require.False(t, called)
		require.Zero(t, perms.requestCount)
	}
	tool, perms := newRunCommandToolWithRecordingPerms(t, t.TempDir())
	perms.allow = false
	called := false
	resp := runRunCommandTool(t, tool, WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(*ArgvProcess) { called = true }}), argvParams(t, "output"))
	require.True(t, resp.IsError)
	require.False(t, called)
	require.Equal(t, 1, perms.requestCount)
	perms.allow = true
	resp = runRunCommandTool(t, tool, WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(*ArgvProcess) { called = true }}), argvParams(t, "output"))
	require.False(t, resp.IsError)
	require.True(t, called)
	require.Equal(t, 2, perms.requestCount)

	called = false
	resp = runRunCommandTool(t, tool, WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(*ArgvProcess) { called = true }}), RunCommandParams{Program: "missing-argv-leaf-binary"})
	require.True(t, resp.IsError)
	require.False(t, called)
	raw, err := json.Marshal(argvParams(t, "output"))
	require.NoError(t, err)
	_, err = tool.Run(WithArgvLeaf(context.Background(), &ArgvLeaf{}), fantasy.ToolCall{Input: string(raw)})
	require.ErrorContains(t, err, "session ID")
}

type argvToolResult struct {
	response fantasy.ToolResponse
	err      error
}

func startArgvTest(t *testing.T, ctx context.Context, mode string) (*ArgvProcess, <-chan argvToolResult) {
	t.Helper()
	tool := newRunCommandToolForTest(t, t.TempDir())
	started := make(chan *ArgvProcess, 1)
	result := make(chan argvToolResult, 1)
	params := argvParams(t, mode)
	params.TimeoutSeconds = 1 // Leaf must not install this tool's timer.
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	ctx = WithArgvLeaf(ctx, &ArgvLeaf{OnStart: func(p *ArgvProcess) { started <- p }})
	go func() {
		resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(raw)})
		result <- argvToolResult{resp, err}
	}()
	select {
	case p := <-started:
		t.Cleanup(func() { _ = p.Stop(context.Background()); <-p.Done() })
		return p, result
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not publish")
	}
	return nil, nil
}

func TestRunCommandLeafCancelTree(t *testing.T) {
	ctx, cancel := context.WithCancel(argvContext())
	defer cancel()
	p, result := startArgvTest(t, ctx, "tree")
	readCtx, end := context.WithTimeout(t.Context(), 15*time.Second)
	defer end()
	text := ""
	cursor := int64(0)
	for !strings.Contains(text, "parent-ready") || !strings.Contains(text, "child-ready") {
		chunk, err := p.ReadOutput(readCtx, cursor)
		require.NoError(t, err)
		require.False(t, chunk.EOF)
		cursor = int64(chunk.Next)
		text += string(chunk.Data)
	}
	cancel()
	select {
	case r := <-result:
		require.ErrorIs(t, r.err, context.Canceled)
	case <-readCtx.Done():
		t.Fatal("tree Wait did not drain")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("tool returned before drained done")
	}
	require.NoError(t, p.Stop(t.Context()))
	require.NoError(t, p.Stop(t.Context()))
	chunk, err := p.ReadOutput(t.Context(), 0)
	require.NoError(t, err)
	require.True(t, chunk.EOF)
	require.Contains(t, string(chunk.Data), "child-ready")
	require.NoError(t, p.Dispose(t.Context()))
}

func TestRunCommandLeafNoDefaultTimer(t *testing.T) {
	p, result := startArgvTest(t, argvContext(), "hold")
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	chunk, err := p.ReadOutput(ctx, 0)
	require.NoError(t, err)
	require.Contains(t, string(chunk.Data), "child-ready")
	_, err = p.ReadOutput(ctx, int64(chunk.Next))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-p.Done():
		t.Fatal("leaf installed tool termination timer")
	default:
	}
	require.NoError(t, p.Stop(t.Context()))
	select {
	case r := <-result:
		require.NoError(t, r.err)
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not drain")
	}
}

func controlledArgvProcess(kill func() error, cap int) *ArgvProcess {
	b := newRunCommandOutputBuffer(cap)
	p := &ArgvProcess{buf: b, done: make(chan struct{}), stopSlot: make(chan struct{}, 1), kill: kill}
	p.stopSlot <- struct{}{}
	b.changed = make(chan struct{})
	return p
}

func TestRunCommandLeafStopAckIsNotCompletion(t *testing.T) {
	p := controlledArgvProcess(func() error { return nil }, 10)
	require.NoError(t, p.Stop(t.Context()))
	select {
	case <-p.Done():
		t.Fatal("Stop acknowledgement synthesized termination")
	default:
	}
	_, err := p.Outcome()
	require.ErrorIs(t, err, ErrArgvNotDone, "Stop acknowledgement is not a real Wait outcome")
	require.ErrorIs(t, p.Dispose(t.Context()), ErrArgvNotDone, "Dispose must refuse before real Done")
	close(p.done)
	require.NoError(t, p.Dispose(t.Context()))
}

func TestRunCommandLeafStopErrorsTimeoutSerialization(t *testing.T) {
	sentinel := errors.New("physical kill failed")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	p := controlledArgvProcess(func() error { entered <- struct{}{}; <-release; return sentinel }, 10)
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { first <- p.Stop(ctx) }()
	<-entered
	cancel()
	require.ErrorIs(t, <-first, context.Canceled)
	select {
	case <-p.Done():
		t.Fatal("timeout synthesized completion")
	default:
	}
	require.ErrorIs(t, p.Dispose(t.Context()), ErrArgvNotDone)
	// A retry cannot enter while the timed-out physical kill is active.
	retryCtx, retryCancel := context.WithCancel(t.Context())
	retry := make(chan error, 1)
	go func() { retry <- p.Stop(retryCtx) }()
	retryCancel()
	require.ErrorIs(t, <-retry, context.Canceled)
	select {
	case <-entered:
		t.Fatal("overlapping physical kill")
	default:
	}
	// A deadline likewise cannot release the still-active physical slot.
	deadlineCtx, deadlineCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer deadlineCancel()
	require.ErrorIs(t, p.Stop(deadlineCtx), context.DeadlineExceeded)
	select {
	case <-entered:
		t.Fatal("deadline released physical kill slot")
	default:
	}
	// Completion does not free an active physical slot; Dispose waits bounded.
	close(p.done)
	disposeCtx, disposeCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer disposeCancel()
	require.ErrorIs(t, p.Dispose(disposeCtx), context.DeadlineExceeded)
	close(release)
	<-p.stopSlot
	p.stopSlot <- struct{}{}
	require.NoError(t, p.Stop(t.Context()))
	require.NoError(t, p.Dispose(t.Context()))

	p = controlledArgvProcess(func() error { return sentinel }, 10)
	require.ErrorIs(t, p.Stop(t.Context()), sentinel)
	require.ErrorIs(t, p.Stop(t.Context()), sentinel)
	select {
	case <-p.Done():
		t.Fatal("kill error synthesized completion")
	default:
	}
	close(p.done)
	require.NoError(t, p.Dispose(t.Context()))
}

func TestRunCommandLeafCancelHeldKillStillWaitsRealDone(t *testing.T) {
	ctx, cancel := context.WithCancel(argvContext())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var p *ArgvProcess
	tool := newRunCommandToolForTest(t, t.TempDir())
	raw, err := json.Marshal(argvParams(t, "hold"))
	require.NoError(t, err)
	ctx = WithArgvLeaf(ctx, &ArgvLeaf{OnStart: func(process *ArgvProcess) {
		p = process
		realKill := p.kill
		p.kill = func() error { err := realKill(); close(entered); <-release; return err }
		cancel()
	}})
	result := make(chan argvToolResult, 1)
	go func() {
		resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(raw)})
		result <- argvToolResult{resp, err}
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not request Stop")
	}
	select {
	case r := <-result:
		require.ErrorIs(t, r.err, context.Canceled)
	case <-time.After(15 * time.Second):
		t.Fatal("held physical kill obscured real completion")
	}
	disposeCtx, disposeCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer disposeCancel()
	require.ErrorIs(t, p.Dispose(disposeCtx), context.DeadlineExceeded)
	outcome, err := p.Outcome()
	require.NoError(t, err)
	require.Error(t, outcome.WaitErr)
	close(release)
	<-p.stopSlot
	p.stopSlot <- struct{}{}
	require.NoError(t, p.Dispose(t.Context()))
}

func TestRunCommandLeafDisposeLatePhysicalKill(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			tool := newRunCommandToolForTest(t, t.TempDir())
			started := make(chan *ArgvProcess, 1)
			killed, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			raw, err := json.Marshal(argvParams(t, "hold"))
			require.NoError(t, err)
			ctx := WithArgvLeaf(argvContext(), &ArgvLeaf{OnStart: func(p *ArgvProcess) {
				realKill := p.kill
				p.kill = func() error { err := realKill(); close(killed); <-release; return err }
				started <- p
			}})
			result := make(chan argvToolResult, 1)
			go func() {
				resp, err := tool.Run(ctx, fantasy.ToolCall{Input: string(raw)})
				result <- argvToolResult{resp, err}
			}()
			p := <-started
			require.ErrorIs(t, p.Dispose(t.Context()), ErrArgvNotDone)
			stopCtx, stopCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer stopCancel()
			require.ErrorIs(t, p.Stop(stopCtx), context.DeadlineExceeded)
			<-killed
			select {
			case r := <-result:
				require.NoError(t, r.err)
			case <-time.After(15 * time.Second):
				t.Fatal("Run did not complete while kill acknowledgement held")
			}
			<-p.Done()
			if expired {
				disposeCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, p.Dispose(disposeCtx), context.DeadlineExceeded)
				chunk, err := p.ReadOutput(t.Context(), 0)
				require.NoError(t, err)
				require.True(t, chunk.EOF)
			}
			answer := make(chan error, 1)
			go func() { answer <- p.Dispose(t.Context()) }()
			select {
			case err := <-answer:
				t.Fatalf("Dispose must wait for physical kill, got %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			// Stop needs mu while Dispose is waiting for the slot.
			p.mu.Lock()
			disposed := p.disposed
			p.mu.Unlock()
			require.False(t, disposed, "output must remain retained while Dispose waits")
			release <- struct{}{}
			require.NoError(t, <-answer)
			require.ErrorIs(t, p.Dispose(t.Context()), ErrArgvDisposed)
		})
	}
}

func TestRunCommandLeafOutputCursorsAndBlocking(t *testing.T) {
	p := controlledArgvProcess(func() error { return nil }, 5)
	_, err := p.buf.Write([]byte("01234567"))
	require.NoError(t, err)
	for _, cursor := range []int64{0, -1, 999} {
		chunk, err := p.ReadOutput(t.Context(), cursor)
		require.NoError(t, err)
		require.Equal(t, "34567", string(chunk.Data))
		require.EqualValues(t, 3, chunk.Oldest)
		require.EqualValues(t, 8, chunk.Next)
		require.True(t, chunk.Truncated)
		require.False(t, chunk.EOF)
	}
	result := make(chan string, 1)
	go func() {
		chunk, err := p.ReadOutput(t.Context(), 8)
		if err != nil {
			result <- err.Error()
		} else {
			result <- string(chunk.Data)
		}
	}()
	select {
	case <-result:
		t.Fatal("head read did not block")
	default:
	}
	_, err = p.buf.Write([]byte("AB"))
	require.NoError(t, err)
	require.Equal(t, "AB", <-result)
	close(p.done)
	chunk, err := p.ReadOutput(t.Context(), 8)
	require.NoError(t, err)
	require.Equal(t, "AB", string(chunk.Data))
	require.True(t, chunk.EOF)
	chunk, err = p.ReadOutput(t.Context(), int64(chunk.Next))
	require.NoError(t, err)
	require.Empty(t, chunk.Data)
	require.True(t, chunk.EOF)
	require.NoError(t, p.Dispose(t.Context()))
}
