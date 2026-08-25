package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestP723_TryRegisterRefusesAfterHubCommittedToExit verifies that once
// Hub.Run's ctx.Done() case has closed stopped, tryRegister returns false
// even with a fresh uncancelled context — the stopped check alone refuses the
// registration.
func TestP723_TryRegisterRefusesAfterHubCommittedToExit(t *testing.T) {
	t.Parallel()

	hub := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		hub.Run(ctx)
		close(runDone)
	}()

	cancel()
	<-hub.stopped
	<-runDone

	c := newClient(hub, nil)
	require.False(t, hub.tryRegister(context.Background(), c),
		"tryRegister must refuse after hub.stopped is closed")
	require.Zero(t, len(hub.register),
		"refused client must not be left in the register channel")
	close(c.workQueue)
}

// TestP723_RunClosesSendOfLateBufferedRegistrations is the regression test
// for the Run-side drain: with the old code, whenever Run's random select
// picked ctx.Done() first with a client already buffered in register, that
// client was never drained — it sat in the channel forever, leaking its
// goroutines and socket. The new code's explicit drain (for c := <-h.register)
// runs BEFORE the active-client close loop and closes c.send, so the client's
// writePump (if it started) or any post-drain registration sees the closed
// channel and exits cleanly.
//
// This test is deterministic on the fixed code (the pre-cancelled ctx forces
// Run into its shutdown path, and we assert the buffered client's send is
// closed). On the old code, it would fail with probability 1-2^-20 across
// 20 iterations because the random select could pick ctx.Done() first every
// time, leaving the client undrained.
func TestP723_RunClosesSendOfLateBufferedRegistrations(t *testing.T) {
	t.Parallel()

	for i := 0; i < 20; i++ {
		hub := newHub()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		c := newClient(hub, nil)
		// Buffer the client before Run starts — the exact race scenario:
		// register has space, ctx is cancelled, and Run hasn't drained yet.
		hub.register <- c

		runDone := make(chan struct{})
		go func() {
			hub.Run(ctx)
			close(runDone)
		}()

		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Fatal("hub.Run did not return within timeout")
		}

		// The client's send channel must be closed by Run's drain loop,
		// not left open (which would leak the writePump goroutine).
		select {
		case _, ok := <-c.send:
			require.False(t, ok, "client's send channel must be closed by Run's drain")
		default:
			t.Fatal("client's send channel left open — registration drained by nobody")
		}

		close(c.workQueue)
	}
}

// TestP723_TryRegisterNoLeakWhenHubStopsDuringInFlightSend is the integration test
// for the full race: a send parks on a full buffer, Run starts with a
// pre-cancelled ctx, and the send unblocks while Run is in its shutdown path.
// The invariant is that there is no leak regardless of which arm wins:
//   - If tryRegister returns true (send completed before the post-send check
//     observed stopped closed), the client was in the channel when Run's drain
//     ran, and Run deterministically closes c.send before returning.
//   - If tryRegister returns false (either pre-check observed stopped closed,
//     or post-send observed it), the caller tears down and c.send is GC'd.
//
// This test accepts both outcomes and verifies the invariant for each.
// It runs 20 iterations to exercise both arms across runs (Run's entry
// select randomly interleaves register service with ctx.Done() when the
// buffer is non-empty).
func TestP723_TryRegisterNoLeakWhenHubStopsDuringInFlightSend(t *testing.T) {
	t.Parallel()

	var trueCount, falseCount int

	for i := 0; i < 20; i++ {
		hub := newHub()

		// Fill register to capacity so the real test client's send parks.
		for i := 0; i < cap(hub.register); i++ {
			d := newClient(hub, nil)
			close(d.workQueue)
			hub.register <- d
		}

		c := newClient(hub, nil)
		res := make(chan bool, 1)

		// Start the registration in its own goroutine — it will block on the
		// full buffer until Run's drain frees a slot.
		go func() {
			res <- hub.tryRegister(context.Background(), c)
		}()

		// Run with a pre-cancelled context so it immediately enters shutdown,
		// closes stopped, and begins draining the register buffer.
		runCtx, cancel := context.WithCancel(context.Background())
		cancel()
		runDone := make(chan struct{})
		go func() {
			hub.Run(runCtx)
			close(runDone)
		}()

		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Fatal("hub.Run did not return within timeout")
		}

		ok := <-res
		if ok {
			trueCount++
			// When true, the send completed before the post-send check observed
			// stopped closed, so the client was in the channel when Run's drain
			// ran. Run's teardown (either the drain itself or the clients-map
			// sweep) must have closed c.send before returning — this is the
			// invariant the bug violated.
			select {
			case _, chanOk := <-c.send:
				require.False(t, chanOk, "client's send channel must be closed by Run's teardown when tryRegister returns true")
			default:
				t.Fatal("client's send channel left open after Run returned — leak")
			}
		} else {
			falseCount++
			// When false, either the pre-check or post-check observed stopped
			// closed. The caller tears down via conn.Close() + close(workQueue);
			// c.send is simply GC'd (writePump was never started). No assertion
			// needed here.
		}

		close(c.workQueue)
	}

	t.Logf("Outcome distribution over 20 runs: true=%d, false=%d (both arms are legitimate)", trueCount, falseCount)
}

// TestP723_TryRegisterPostSendCheckCatchesLateLanding is a white-box,
// deterministic repro of the exact race window the review reported:
// the send completes only AFTER the hub has already closed stopped.
// This test explicitly orders close-before-slot-free, which cannot be
// guaranteed with the real Run loop (parking requires a full buffer;
// a full buffer makes Run's entry select random). We simulate Run's
// shutdown path directly: close stopped, then drain one slot from register.
// The parked send completes into that freed slot, but the post-send check
// must observe stopped closed and return false.
func TestP723_TryRegisterPostSendCheckCatchesLateLanding(t *testing.T) {
	t.Parallel()

	hub := newHub()

	// Fill register to capacity to force the sender to park.
	for i := 0; i < cap(hub.register); i++ {
		d := newClient(hub, nil)
		close(d.workQueue)
		hub.register <- d
	}

	c := newClient(hub, nil)
	res := make(chan bool, 1)

	// Start the registration in its own goroutine — it will block on the
	// full buffer until we free a slot below.
	go func() {
		res <- hub.tryRegister(context.Background(), c)
	}()

	// Pragmatic park wait: Go provides no way to observe that a send is
	// parked from outside, so we wait ~5 orders of magnitude above goroutine
	// scheduling latency. If the sender is nonetheless not yet parked, the
	// test still passes via the pre-check arm — only the len assertion below
	// would show 63 instead of 64, and both are acceptable refusals.
	time.Sleep(50 * time.Millisecond)

	// Simulate Run's commit point: close stopped before draining.
	close(hub.stopped)

	// Simulate Run's drain: receive one dummy, freeing exactly one slot.
	// The parked send now completes into the freed slot.
	<-hub.register

	// The send may have completed into the draining channel, but the
	// post-send stopped check must turn it into a refusal.
	select {
	case ok := <-res:
		require.False(t, ok,
			"send completed after stopped was closed, so post-send check must observe it and return false")
	case <-time.After(5 * time.Second):
		t.Fatal("tryRegister did not return within timeout")
	}

	// Log which arm caught the race: 64 means the send completed and
	// landed in the buffer (post-check arm), 63 means the pre-check caught
	// it before the send even completed. Both are correct refusals.
	t.Logf("register len after: %d (64 = post-send arm, 63 = pre-check arm)", len(hub.register))

	close(c.workQueue)
}

// TestP723_TryRegisterUnblockedByContextCancel verifies the ctx.Done()
// fallback branch: when register is full and stopped is still open (hub is
// still running), tryRegister must immediately return false if ctx is
// cancelled, unblocking the caller without ever sending.
func TestP723_TryRegisterUnblockedByContextCancel(t *testing.T) {
	t.Parallel()

	hub := newHub()

	// Fill register to capacity.
	for i := 0; i < cap(hub.register); i++ {
		d := newClient(hub, nil)
		close(d.workQueue)
		hub.register <- d
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newClient(hub, nil)
	// Buffer is full, stopped is open, ctx is cancelled — the Done branch
	// fires immediately, no goroutine needed.
	require.False(t, hub.tryRegister(ctx, c),
		"tryRegister must return false when ctx is cancelled, unblocking the caller")

	close(c.workQueue)
}
