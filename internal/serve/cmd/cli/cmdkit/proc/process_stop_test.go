package cliproc

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProcessStopFirstRequest(t *testing.T) {
	var calls []string
	var deadline func()
	stop := newProcessStop(func() { calls = append(calls, "trees") }, func(code int) {
		if code != 130 {
			t.Errorf("exit code=%d", code)
		}
		calls = append(calls, "exit")
	}, func(f func()) func() {
		deadline = f
		return func() { calls = append(calls, "disarm") }
	})
	stop(130)
	stop(143)
	if deadline == nil || len(calls) != 3 || calls[0] != "trees" || calls[1] != "exit" || calls[2] != "disarm" {
		t.Fatalf("first request did not exit once after tree teardown: %v", calls)
	}
}

func TestProcessStopDeadlineDoesNotWaitForTeardown(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	armed := make(chan func(), 1)
	var exits atomic.Int64
	stop := newProcessStop(func() { close(entered); <-release }, func(int) { exits.Add(1) }, func(f func()) func() {
		armed <- f
		return func() {}
	})
	go func() { defer close(done); stop(143) }()
	t.Cleanup(func() { close(release); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never reached tree teardown")
	}
	select {
	case fire := <-armed:
		fire()
	case <-time.After(5 * time.Second):
		t.Fatal("exit deadline was not armed before teardown")
	}
	if exits.Load() != 1 {
		t.Fatal("deadline did not exit while teardown was blocked")
	}
}

func TestProcessStopOwnsCommandSignals(t *testing.T) {
	restore := SetProcessStopInstalledForTests(true)
	t.Cleanup(restore)
	if got := CommandSignals(os.Interrupt, syscall.SIGTERM); len(got) != 0 {
		t.Fatalf("competing command signals: %v", got)
	}
	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := CommandSignalContext(parent, os.Interrupt)
	defer stop()
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("caller cancellation must still propagate")
	}
}
