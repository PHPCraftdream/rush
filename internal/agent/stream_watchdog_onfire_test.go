// Tests for the onFire/cancel ordering on every fire path: onFire must
// complete before cancel(), the cause must be stored before slow diagnostic
// work runs, and async diagnostic work must never delay cancellation.

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestStreamWatchdog_OnFireCompletesBeforeCancel is the regression test for
// task #227: startStreamWatchdog's doc comment says onFire is invoked AFTER
// stalled is set to true and BEFORE cancel(), but until this fix all 5 fire
// sites actually did `stalled.Store(true); cancel(); onFire(...)` — the
// OPPOSITE order. That let a second goroutine blocked on <-ctx.Done() race
// ahead and observe cancellation before onFire (which, in agent.go, stores
// the real watchdogCause AFTER a slow disk-writing goroutine dump) had
// finished recording anything — silently defeating task #223's cause
// attribution on a genuine hard-cap/tool-timeout fire.
//
// This proves the fix deterministically (no time.Sleep guessing): onFire
// blocks on a channel until the test goroutine has confirmed ctx is NOT YET
// Done, then signals onFire to proceed and records the moment it returns.
// The test asserts, via a channel handshake instead of timing, that
// ctx.Done() cannot be observed as closed until AFTER onFire has fully
// returned.
func TestStreamWatchdog_OnFireCompletesBeforeCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 20 * time.Millisecond
	const tick = 5 * time.Millisecond

	// onFireEntered is closed the instant onFire starts running.
	onFireEntered := make(chan struct{})
	// releaseOnFire is closed by the test goroutine once it has confirmed
	// ctx is still NOT Done — only then is onFire allowed to return.
	releaseOnFire := make(chan struct{})
	// onFireReturned is closed right after onFire returns, i.e. right
	// before the watchdog goroutine calls cancel().
	onFireReturned := make(chan struct{})

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		close(onFireEntered)
		<-releaseOnFire
		close(onFireReturned)
	}, false, 0, 0, 0, nil)

	// Wait for onFire to start.
	select {
	case <-onFireEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire was never invoked")
	}

	// While onFire is blocked (has not returned), ctx must NOT be Done yet
	// — cancel() must not have run. This is the deterministic core of the
	// assertion: sampled precisely while onFire is known to still be
	// in-flight, not via a timing guess.
	select {
	case <-ctx.Done():
		t.Fatal("ctx was cancelled before onFire returned — cancel() must run strictly after onFire completes")
	default:
		// Good: ctx is still live while onFire is blocked.
	}
	assert.NoError(t, ctx.Err(), "ctx must not be cancelled while onFire is still running")

	// Now let onFire finish, and confirm ctx becomes Done only afterward.
	close(releaseOnFire)

	select {
	case <-onFireReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire never returned after being released")
	}

	// ctx.Done() must become observable (cancel() called) after onFire
	// returned. Poll done deterministically via the watchdog's own done
	// channel, which only closes after the fire-site's cancel()+return.
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited after onFire returned")
	}
	assert.Error(t, ctx.Err(), "ctx must be cancelled after onFire returned")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork is the regression
// test for task #232 issue 1. It mirrors agent.go's actual onFire closure
// shape: store the fire cause FIRST, synchronously, then do the (potentially
// slow) diagnostic work. Because task #227 made onFire run strictly before
// cancel(), an external cancellation of ctx (independent of this watchdog —
// e.g. user Ctrl-C, a --timeout racing the same instant) could previously be
// observed by another goroutine while the cause store hadn't happened yet
// if the cause store came after the slow work, misattributing the fire.
//
// This test proves the ordering deterministically (no time.Sleep guessing):
// the simulated diagnostic work blocks on a channel, and the test asserts
// the cause is already readable as the real fired cause WHILE that
// diagnostic work is still blocked — not just after onFire eventually
// returns.
func TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second
	const tick = 5 * time.Millisecond

	var causeVal atomic.Int32
	causeVal.Store(int32(causeIdleStall)) // zero value; must be overwritten by the real cause

	diagnosticWorkEntered := make(chan struct{})
	releaseDiagnosticWork := make(chan struct{})

	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		// Mirrors agent.go's onFire: store the cause FIRST, synchronously,
		// before any slow diagnostic work.
		causeVal.Store(int32(cause))
		close(diagnosticWorkEntered)
		<-releaseDiagnosticWork
	},
		false, 0,
		1*time.Millisecond, // toolMaxDuration: tiny, so a tool-in-flight fires with causeToolTimeout
		0, nil)

	// Put a tool in flight so the watchdog fires with causeToolTimeout, not
	// causeIdleStall — this makes the assertion meaningful: if the store
	// were still reachable only after the slow work, we'd see the zero
	// value (causeIdleStall) instead.
	wd.toolStarted()

	select {
	case <-diagnosticWorkEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire's diagnostic work was never entered")
	}

	// While the diagnostic work is still blocked, the cause must already be
	// the real fired cause (causeToolTimeout), not the zero value.
	assert.Equal(t, int32(causeToolTimeout), causeVal.Load(),
		"cause must be stored before the slow diagnostic work runs, so a concurrent reader "+
			"never observes the zero value for a real tool-timeout/hard-cap fire")

	close(releaseDiagnosticWork)
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited")
	}
}

// TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel is the regression
// test for task #232 issue 2. Since task #227, onFire runs strictly before
// cancel() on every fire path — so a slow or hung disk write inside onFire
// (DumpGoroutines in production, see internal/log/goroutine_dump.go, has no
// timeout of its own) would previously delay cancel() by however long the
// write takes, or block it forever if the write hangs. agent.go's fix
// dispatches the diagnostic work in its own goroutine and returns from
// onFire immediately, without awaiting it.
//
// This test proves that pattern deterministically: onFire launches a
// goroutine that blocks on a channel the test never closes (simulating a
// permanently hung write) and returns immediately. The watchdog's own
// cancel()+done must complete promptly regardless — proving the hang can
// never delay cancellation, not just that it usually doesn't.
func TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 20 * time.Millisecond
	const tick = 5 * time.Millisecond

	// neverReleased is intentionally never closed, simulating a diagnostic
	// write that hangs forever (e.g. a stuck network/SMB mount).
	neverReleased := make(chan struct{})
	diagnosticStarted := make(chan struct{})
	t.Cleanup(func() {
		// Let the leaked goroutine exit at test end rather than leaking past
		// this test (it would otherwise block until process exit).
		close(neverReleased)
	})

	onFireReturned := make(chan struct{})
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		go func() {
			close(diagnosticStarted)
			<-neverReleased // never closed by the test — simulates a hung write
		}()
		close(onFireReturned)
	}, false, 0, 0, 0, nil)

	// onFire must return promptly even though the diagnostic goroutine it
	// launched will never finish.
	select {
	case <-onFireReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("onFire did not return — async diagnostic work must not be awaited")
	}
	select {
	case <-diagnosticStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the diagnostic goroutine was never scheduled")
	}

	// cancel()/done must complete promptly — NOT blocked on the permanently
	// hung diagnostic goroutine.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx was never cancelled — a hung async diagnostic write must not block cancel()")
	}
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine never exited — a hung async diagnostic write must not block it")
	}
	assert.True(t, wd.stalled.Load())
}
