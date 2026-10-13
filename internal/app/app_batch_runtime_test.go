// Fake Runner and shared helpers for the R11E host-lifecycle tests.

package app

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRunner is a controllable batchRunner: tests fire faults explicitly and
// script Close (including a never-returning hook shape).
type fakeRunner struct {
	faulted  chan struct{}
	faultErr error

	closeMu    sync.Mutex
	closeHook  func(ctx context.Context) error
	closeErr   error
	closeCalls atomic.Int32
	// closeEntered is sent when Close starts, for sleep-free ordering.
	closeEntered chan struct{}
	// closeRelease gates Close's return: Close blocks until it is closed or
	// ctx is done.
	closeRelease chan struct{}
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		faulted:      make(chan struct{}),
		closeEntered: make(chan struct{}, 1),
		closeRelease: make(chan struct{}),
	}
}

func (f *fakeRunner) Faulted() <-chan struct{} { return f.faulted }

func (f *fakeRunner) FaultErr() error { return f.faultErr }

func (f *fakeRunner) FireFault(err error) {
	f.faultErr = err
	close(f.faulted)
}

func (f *fakeRunner) Close(ctx context.Context) error {
	f.closeCalls.Add(1)
	select {
	case f.closeEntered <- struct{}{}:
	default:
	}
	f.closeMu.Lock()
	hook := f.closeHook
	f.closeMu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	select {
	case <-f.closeRelease:
		return f.closeErr
	case <-ctx.Done():
		return f.closeErr
	}
}

// finishClose unblocks Close with the scripted result.
func (f *fakeRunner) finishClose(err error) {
	f.closeErr = err
	select {
	case <-f.closeRelease:
	default:
		close(f.closeRelease)
	}
}

// immediateWaitFunc returns a WaitFunc whose containment bound expires
// instantly — ordering tests without sleeps.
func immediateWaitFunc() func(time.Duration) <-chan time.Time {
	return func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
}

// exitRecorder captures the injectable exit calls.
type exitRecorder struct {
	mu    sync.Mutex
	codes []int
}

func (e *exitRecorder) exit(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.codes = append(e.codes, code)
}

func (e *exitRecorder) called() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.codes...)
}

// releaseRecorder records the call order of named release steps.
type releaseRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *releaseRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, name)
}

func (r *releaseRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func newTestRuntime(t *testing.T, runner *fakeRunner) (*BatchRuntime, *exitRecorder, *releaseRecorder) {
	t.Helper()
	rec := &releaseRecorder{}
	exits := &exitRecorder{}
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Second,
		WaitFunc:        immediateWaitFunc(),
		ExitFunc:        exits.exit,
		RetainHostIdentity: func() {
			rec.record("retain-host")
		},
		ReleaseHostIdentity: func(context.Context) error {
			rec.record("release-host")
			return nil
		},
		ReleaseWriter: func(context.Context) error {
			rec.record("release-writer")
			return nil
		},
	})
	if runner != nil {
		rt.SetRunner(runner)
	}
	return rt, exits, rec
}

// TestBatchRuntimeConstructionHasNoGoroutines proves construction has no
// side effects: no watcher goroutine, no releases, no exit. It counts only
// the runtime's own watcher goroutines (by stack), not process-wide ones, and
// is not parallel: other tests' goroutines start and exit around it.
func TestBatchRuntimeConstructionHasNoGoroutines(t *testing.T) {
	before := watcherGoroutines()
	rt := NewBatchRuntime(BatchRuntimeConfig{})
	require.Equal(t, before, watcherGoroutines(), "construction must not start goroutines")
	require.Nil(t, rt.Runner(), "dormant runtime has no Runner")
	require.False(t, rt.Fenced(), "dormant runtime is not fenced")
	require.Nil(t, rt.cfg.ReleaseWriter, "construction must not invent release hooks")
}

// allStacks returns a full process goroutine stack dump, growing the buffer
// until the whole dump fits.
func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// watcherGoroutines counts goroutines currently inside the fault watcher.
func watcherGoroutines() int {
	return strings.Count(allStacks(), "BatchRuntime).watchFaults")
}

// watcherParkedInSelect reports whether the watcher goroutine is currently
// blocked in a select. While no Runner is attached the live fault select is
// unreachable (rt.runner is nil), so a parked watcher is necessarily in the
// dormant runnerSet select.
func watcherParkedInSelect() bool {
	// runtime.Stack hides runtime-internal frames (selectgo), but each
	// goroutine block header carries its wait state: "goroutine N [select]:".
	for _, block := range strings.Split(allStacks(), "\n\n") {
		if strings.Contains(block, "BatchRuntime).watchFaults") && strings.Contains(firstLine(block), "[select") {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
