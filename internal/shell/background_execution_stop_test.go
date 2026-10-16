package shell

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestRetainedStopAcknowledgmentAndPhysicalError(t *testing.T) {
	bs, execution := heldExecution()
	physicalErr := errors.New("physical kill refused")
	var calls atomic.Int32
	p := &retainedProcess{kill: func() error { calls.Add(1); return physicalErr }}
	execution.physical = &retainedStops{entries: map[*retainedProcess]struct{}{p: {}}}
	require.ErrorIs(t, execution.Stop(t.Context()), physicalErr)
	require.False(t, bs.IsDone(), "physical failure is not termination")
	p.mu.Lock()
	p.kill = func() error { calls.Add(1); return nil }
	p.mu.Unlock()
	require.NoError(t, execution.Stop(t.Context()))
	require.False(t, bs.IsDone(), "acknowledgment must not wait for or synthesize done")
	require.EqualValues(t, 2, calls.Load())
	require.NoError(t, execution.Stop(t.Context()))
	require.EqualValues(t, 2, calls.Load(), "successful acknowledgment is cached")
	require.ErrorIs(t, execution.Dispose(t.Context()), ErrExecutionNotDone)
	close(bs.done)
	require.NoError(t, execution.Dispose(t.Context()))
}

func TestRetainedStopHeldControlAndDisposeBarrier(t *testing.T) {
	bs, execution := heldExecution()
	entered, release := make(chan struct{}), make(chan struct{})
	p := &retainedProcess{kill: func() error { close(entered); <-release; return nil }}
	execution.physical = &retainedStops{entries: map[*retainedProcess]struct{}{p: {}}}
	first := make(chan error, 1)
	go func() { first <- execution.Stop(t.Context()) }()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, execution.Stop(ctx), context.Canceled)
	require.ErrorIs(t, execution.Dispose(t.Context()), ErrExecutionNotDone)
	close(bs.done)
	disposed := make(chan error, 1)
	go func() { disposed <- execution.Dispose(t.Context()) }()
	select {
	case <-disposed:
		t.Fatal("Dispose passed a physically held Stop")
	default:
	}
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-disposed)
}

func TestRetainedPhysicalStopRealProcess(t *testing.T) {
	if os.Getenv("RUSH_RETAINED_STOP_HELPER") == "1" {
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		return
	}
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := platform.Command(context.Background(), self, "-test.run=^TestRetainedPhysicalStopRealProcess$")
	cmd.Env = append(os.Environ(), "RUSH_RETAINED_STOP_HELPER=1")
	isolateProcess(cmd)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer stdin.Close()
	stops := &retainedStops{entries: make(map[*retainedProcess]struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	killed := make(chan struct{})
	registered := make(chan struct{})
	waited := make(chan error, 1)
	go func() {
		waited <- runRetainedProcess(ctx, cmd, stops, func() error {
			defer close(killed)
			return cmd.Process.Kill()
		}, func(int) { close(registered) })
	}()
	<-registered
	cancel()
	select {
	case <-killed:
	case <-time.After(10 * time.Second):
		t.Fatal("physical cancellation callback did not return")
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("killed helper did not return from Wait")
	}
}
