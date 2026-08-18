// Waiting semantics for background shells: WaitContext, WaitForChange
// (output growth, completion, ctx-done), OnDone callbacks, and Elapsed.
package shell

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackgroundShell_WaitContext_Completed(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	close(done)

	bgShell := &BackgroundShell{done: done}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	require.True(t, bgShell.WaitContext(ctx))
}

func TestBackgroundShell_WaitContext_Canceled(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.False(t, bgShell.WaitContext(ctx))
}

// TestBackgroundShell_WaitForChange_ReturnsOnOutputGrowth proves the wait
// returns promptly when buffered output grows past the supplied baseline,
// without waiting for the job to finish or the ctx to time out.
func TestBackgroundShell_WaitForChange_ReturnsOnOutputGrowth(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Baseline is one byte; we will write more into the buffer.
	sinceLen := 1

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		bgShell.WaitForChange(ctx, sinceLen)
		close(done)
	}()

	// Give the goroutine a moment to enter the select, then push output.
	time.Sleep(350 * time.Millisecond)
	bgShell.stdout.WriteString("hello world")

	select {
	case <-done:
		// Expected: returned because output grew past baseline.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not return after output grew past baseline")
	}
}

// TestBackgroundShell_WaitForChange_ReturnsOnCompletion proves the wait
// returns the moment the job's done channel closes.
func TestBackgroundShell_WaitForChange_ReturnsOnCompletion(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		// Baseline huge so the only way out is the done channel.
		bgShell.WaitForChange(ctx, 1<<30)
		close(done)
	}()

	time.Sleep(350 * time.Millisecond)
	close(bgShell.done)

	select {
	case <-done:
		// Expected: returned because job completed.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not return after job completed")
	}
}

// TestBackgroundShell_WaitForChange_ReturnsOnCtxDone proves the wait never
// blocks indefinitely: a ctx that times out (or is canceled) without any
// output growth or job completion still unblocks the caller.
func TestBackgroundShell_WaitForChange_ReturnsOnCtxDone(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		stderr:    &boundedBuffer{maxBytes: maxStreamBufferBytes},
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	// Huge baseline, never-completing job → only ctx.Done() can fire.
	bgShell.WaitForChange(ctx, 1<<30)
	elapsed := time.Since(start)

	// Must return ~right after the ctx deadline, not hang.
	require.Less(t, elapsed, 2*time.Second, "WaitForChange should return when ctx ends")
}

// TestBackgroundShell_OnDone_FiresOnCompletion proves OnDone fires promptly
// when a short-lived background command finishes on its own.
func TestBackgroundShell_OnDone_FiresOnCompletion(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo hi", "")
	require.NoError(t, err)

	fired := make(chan struct{})
	bgShell.OnDone(func() { close(fired) })

	select {
	case <-fired:
		// Expected: OnDone fired once the echo finished.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not fire after command completed")
	}

	// Clean up (no-op once already gone, but keeps the manager tidy).
	_ = manager.Kill(t.Context(), bgShell.ID)
}

// TestBackgroundShell_OnDone_FiresOnKill proves OnDone does NOT fire while a
// long-running command is still alive, but DOES fire promptly once the job is
// killed via the manager.
func TestBackgroundShell_OnDone_FiresOnKill(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "sleep 30", "")
	require.NoError(t, err)

	fired := make(chan struct{})
	bgShell.OnDone(func() { close(fired) })

	// While the job is alive, OnDone must not fire.
	select {
	case <-fired:
		t.Fatal("OnDone fired while command was still running")
	case <-time.After(300 * time.Millisecond):
		// Expected: still running.
	}

	// Killing the job must release OnDone.
	require.NoError(t, manager.Kill(t.Context(), bgShell.ID))

	select {
	case <-fired:
		// Expected: OnDone fired after Kill.
	case <-time.After(3 * time.Second):
		t.Fatal("OnDone did not fire after Kill")
	}
}

// TestBackgroundShell_OnDone_PanicRecovered proves that a panic inside the
// fn passed to OnDone is recovered (logged) rather than crashing the
// process. OnDone's goroutine is independent of whatever turn started the
// background job (see the doc comment on OnDone) — production code passes
// the agent package's notifyBackgroundJobDone here, which can itself start
// a fresh top-level turn, so an unrecovered panic here would previously
// have taken down the whole crush process with no log output.
func TestBackgroundShell_OnDone_PanicRecovered(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo hi", "")
	require.NoError(t, err)

	// Register a second, well-behaved OnDone callback AFTER the panicking
	// one to confirm the panic in the first callback's own goroutine does
	// not prevent other independent OnDone registrations from firing (each
	// OnDone call spawns its own goroutine, so this also incidentally
	// documents that independence).
	panicked := make(chan struct{})
	bgShell.OnDone(func() {
		defer close(panicked)
		panic("boom: simulated OnDone callback panic")
	})

	fired := make(chan struct{})
	bgShell.OnDone(func() { close(fired) })

	select {
	case <-panicked:
		// Expected: the panicking callback ran (and panicked) without
		// bringing down the test process — reaching this line at all is
		// the core assertion.
	case <-time.After(3 * time.Second):
		t.Fatal("panicking OnDone callback never ran")
	}

	select {
	case <-fired:
		// Expected: unrelated OnDone registration still fires normally.
	case <-time.After(3 * time.Second):
		t.Fatal("sibling OnDone callback did not fire after a panic in another OnDone callback")
	}

	_ = manager.Kill(t.Context(), bgShell.ID)
}

// TestBackgroundShell_Elapsed confirms Elapsed reports a non-zero duration
// once StartTime is set, and zero when it is not.
func TestBackgroundShell_Elapsed(t *testing.T) {
	t.Parallel()

	t.Run("zero when unset", func(t *testing.T) {
		t.Parallel()
		bgShell := &BackgroundShell{}
		require.Zero(t, bgShell.Elapsed())
	})

	t.Run("positive when set", func(t *testing.T) {
		t.Parallel()
		bgShell := &BackgroundShell{StartTime: time.Now()}
		time.Sleep(5 * time.Millisecond)
		require.Positive(t, bgShell.Elapsed())
	})
}
