package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakePull is the notice-pull seam. rootDone models a still-running batch
// root; blockedPull makes Pull wait for the test's release gate (signals,
// never sleeps).
type fakePull struct {
	n        int
	err      error
	rootDone chan struct{}
	blocked  bool
	// ignoreCtx makes the blocked select wait ONLY on release: used when the
	// wrapper's own bound must be the single exit (avoids ctx races).
	ignoreCtx  bool
	release    chan struct{}
	pullObs    chan pullObservation
	calls      chan struct{}
	cancelled  chan struct{}
	cancelOnce chan struct{}
}

// pullObservation is the state of the pull's context at the moment Pull ran.
type pullObservation struct {
	err         error
	hasDeadline bool
}

func newFakePull(n int, err error) *fakePull {
	return &fakePull{n: n, err: err, rootDone: make(chan struct{}), pullObs: make(chan pullObservation, 1), calls: make(chan struct{}, 8)}
}

func (f *fakePull) Pull(ctx context.Context) (int, error) {
	f.calls <- struct{}{}
	// Observe the context NOW: the wrapper cancels it when it returns.
	_, hasDeadline := ctx.Deadline()
	select {
	case f.pullObs <- pullObservation{err: ctx.Err(), hasDeadline: hasDeadline}:
	default:
	}
	if f.blocked {
		// Blocked "running batch work": wait for the test's release, or the
		// bound's cancellation.
		select {
		case <-f.release:
		case <-ctx.Done():
			if !f.ignoreCtx {
				return 0, ctx.Err()
			}
			<-f.release
		}
	}
	if f.err != nil {
		return 0, f.err
	}
	return f.n, nil
}

// Acceptance: Drain is nonblocking while a batch root is still running. The
// pull is wedged behind the (never-closed) root Done; the bound makes Drain
// return anyway.
func TestBatchWebDrainNonblockingWithBlockedRoot(t *testing.T) {
	t.Parallel()
	pull := newFakePull(0, nil)
	pull.blocked = true
	pull.ignoreCtx = true // The wrapper's 50ms bound is the ONLY exit.
	pull.release = make(chan struct{})
	t.Cleanup(func() { close(pull.release) }) // Unblock the goroutine at the end.
	results := make(chan error, 1)
	go func() {
		_, err := batchWebDrain(context.Background(), pull, 50*time.Millisecond)
		results <- err
	}()
	select {
	case err := <-results:
		require.ErrorIs(t, err, errBatchDrainPullTimeout, "Drain returned while the root stayed blocked")
	case <-time.After(2 * time.Second):
		t.Fatal("Drain blocked on running batch work")
	}
	select {
	case <-pull.rootDone:
		t.Fatal("the root must still be running")
	default:
	}
}

// A wedged pull must not outlive the caller: Drain returns as soon as the
// caller's context is cancelled, without waiting for the pull.
func TestBatchWebDrainReturnsWhenCallerCancels(t *testing.T) {
	t.Parallel()
	pull := newFakePull(0, nil)
	pull.blocked = true
	pull.release = make(chan struct{})
	t.Cleanup(func() { close(pull.release) })
	caller, cancel := context.WithCancel(context.Background())
	results := make(chan error, 1)
	go func() {
		_, err := batchWebDrain(caller, pull, time.Hour)
		results <- err
	}()
	<-pull.calls // Pull has started.
	cancel()
	select {
	case err := <-results:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Drain ignored the caller's cancellation")
	}
}

// The happy path: Drain forwards the pulled notice count and errors.
func TestBatchWebDrainPullsNotices(t *testing.T) {
	t.Parallel()
	pull := newFakePull(3, nil)
	n, err := batchWebDrain(context.Background(), pull, time.Second)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	pull2 := newFakePull(0, errors.New("store down"))
	_, err = batchWebDrain(context.Background(), pull2, time.Second)
	require.EqualError(t, err, "store down")
}

// The pull runs under a control context that is NOT the caller's: a caller
// already cancelled before the pull started still drains.
func TestBatchWebDrainPullContextNotCallerCancelled(t *testing.T) {
	t.Parallel()
	pull := newFakePull(1, nil)
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := batchWebDrain(caller, pull, time.Second)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	obs := <-pull.pullObs
	require.NoError(t, obs.err, "the pull must run under a non-cancelled control context")
	require.True(t, obs.hasDeadline)
}

func TestBatchWebDrainRequiresPull(t *testing.T) {
	t.Parallel()
	_, err := batchWebDrain(context.Background(), nil, time.Second)
	require.ErrorContains(t, err, "notice pull is required")
}
