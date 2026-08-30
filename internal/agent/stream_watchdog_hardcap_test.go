// Tests for progress-based deadline extension (extendsOnProgress) and the
// --timeout-hard-cap ceiling: progress keeps a turn alive past the idle
// timeout, the cap still fires through it, and the reported cause must
// distinguish hard-cap fires from genuine idle stalls.

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Fork patch: batch 8 — tests for progress-based deadline extension.

// TestStreamWatchdog_ExtendsOnProgress verifies that with extendsOnProgress
// enabled, continuous progress keeps the watchdog alive beyond the original
// idle timeout.
func TestStreamWatchdog_ExtendsOnProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// 3x the original 80/10/500ms/30ms-bump timings (same ratios): observed
	// flaking under CI load ("watchdog must not fire while progress keeps
	// arriving") when a bump got scheduled more than idle late -- a real
	// wall-clock race against actual scheduling jitter, not a logic bug.
	// More absolute headroom, same relative behavior being tested.
	const idle = 240 * time.Millisecond
	const tick = 30 * time.Millisecond
	const hardCap = 1500 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, true, hardCap, 0, 0, nil)
	defer func() {
		cancel()
		<-wd.done
	}()

	// Bump every 90ms for 900ms — extends the deadline each time.
	stop := time.After(900 * time.Millisecond)
loop:
	for {
		select {
		case <-stop:
			break loop
		case <-time.After(90 * time.Millisecond):
			wd.bump()
		}
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while progress keeps arriving")
	assert.False(t, wd.stalled.Load())
}

// TestStreamWatchdog_ExtendsOnProgress_FiresWhenIdle verifies that with
// extendsOnProgress, the watchdog still fires when progress stops.
func TestStreamWatchdog_ExtendsOnProgress_FiresWhenIdle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 500 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, true, hardCap, 0, 0, nil)

	// Bump once to extend, then stop.
	wd.bump()

	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog should have fired after progress stopped")
	}

	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire when progress stops")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeIdleStall, watchdogCause(firedCause.Load()),
		"progress stopping (not a hard cap, not a stuck tool) must report causeIdleStall")
}

// TestStreamWatchdog_HardCapRespected verifies that even with continuous
// progress, the watchdog fires at the hard cap.
func TestStreamWatchdog_HardCapRespected(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 200 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 400 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, true, hardCap, 0, 0, nil)

	start := time.Now()

	// Bump rapidly — but hard cap should still kill it.
	stop := time.After(2 * time.Second)
loop:
	for {
		select {
		case <-wd.done:
			break loop
		case <-stop:
			t.Fatal("watchdog should have fired at hard cap")
		case <-time.After(10 * time.Millisecond):
			wd.bump()
		}
	}

	elapsed := time.Since(start)
	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"firing at the hard cap despite continuous progress must report causeHardCap")
	// The hard cap is 400ms; widened from an earlier 200ms-cap/350ms-ceiling
	// version (task #320) whose 150ms of upper-bound slack was tight enough
	// to flake on a loaded CI runner under -race in a full-package parallel
	// run, where a single delayed tick is enough to blow the margin without
	// indicating any real bug. 800ms of slack on top of the cap only fails
	// if the fire is genuinely late, not merely jittered.
	assert.LessOrEqual(t, elapsed, hardCap+800*time.Millisecond,
		"watchdog must fire near the hard cap")
	// Regression for task #276: the value passed to onFire must be the
	// wall-clock turn length (now.Sub(startTime)), not idle — this loop
	// bumps every 10ms, so idle sits near-zero for the whole run, and a
	// misdiagnosed hard-cap fire would report an elapsed close to 0
	// instead of close to hardCap.
	gotElapsed := time.Duration(firedElapsed.Load())
	assert.GreaterOrEqual(t, gotElapsed, hardCap,
		"elapsed passed to onFire must reflect wall-clock turn length, not near-zero idle time")
}

// TestStreamWatchdog_HardCapRespectedWithoutExtendsOnProgress is the
// regression test for a real bug: the hardCap check used to live ONLY
// inside the `if extendsOnProgress` branch of the idle-path check, so when
// extendsOnProgress is false (the default — operator did not pass
// --timeout-extends-on-progress) an explicitly configured --timeout-hard-cap
// was never enforced as long as the provider kept the stream alive with
// regular chunks: idle never reached idleTimeout, and the hardCap check was
// unreachable on that path. This proves the fix: even with extendsOnProgress
// false and bump() called continuously (idle never approaches idleTimeout),
// the watchdog must still fire at the hard cap.
func TestStreamWatchdog_HardCapRespectedWithoutExtendsOnProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 200 * time.Millisecond
	const tick = 10 * time.Millisecond
	const hardCap = 400 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, false, hardCap, 0, 0, nil)

	start := time.Now()

	// Bump rapidly — more often than idleTimeout — so the idle-only check
	// would NEVER fire on its own. The hard cap must still kill it.
	stop := time.After(2 * time.Second)
loop:
	for {
		select {
		case <-wd.done:
			break loop
		case <-stop:
			t.Fatal("watchdog should have fired at hard cap despite continuous activity")
		case <-time.After(10 * time.Millisecond):
			wd.bump()
		}
	}

	elapsed := time.Since(start)
	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"the hard-cap fire outside tool-in-flight must report causeHardCap, not causeIdleStall or causeToolTimeout")
	// The hard cap is 400ms; widened from an earlier 200ms-cap/350ms-ceiling
	// version (task #320) for the same reason as TestStreamWatchdog_HardCapRespected:
	// 150ms of upper-bound slack was tight enough to flake on a loaded CI
	// runner under -race in a full-package parallel run.
	assert.LessOrEqual(t, elapsed, hardCap+800*time.Millisecond,
		"watchdog must fire near the hard cap")
	// Regression for task #276: this is exactly the branch that used to
	// pass idle instead of the wall-clock elapsed to onFire. The bump loop
	// keeps idle near-zero for the whole run, so a misdiagnosed fire would
	// report an elapsed close to 0 instead of close to hardCap.
	gotElapsed := time.Duration(firedElapsed.Load())
	assert.GreaterOrEqual(t, gotElapsed, hardCap,
		"elapsed passed to onFire must reflect wall-clock turn length, not near-zero idle time")
}

// TestStreamWatchdog_HardCapRespectedWithToolInFlight is the regression test
// for a real bug: the explicit hardCap check inside the toolsInFlight branch
// (the one immediately below the never-freeze toolMaxDuration backstop) used
// to unconditionally report toolTimeout=true to onFire, even though it is
// the SAME wall-clock --timeout-hard-cap check as the one on the idle path.
// 77c1104a fixed the immediate boolean misclassification (both paths now
// agree on toolTimeout=false), but that only made the hard-cap-with-tool
// case indistinguishable from a genuine provider idle-stall instead of from
// a stuck tool — either way onFire couldn't tell "the turn hit its
// configured ceiling" apart from "the provider went silent". This test
// (task #223) proves the fully-fixed three-way signature: with a small
// hardCap and a generous toolMaxDuration (so the never-freeze backstop does
// not fire first), the watchdog must still fire at the hard cap while a
// tool is in flight, and must report the DISTINCT causeHardCap value — not
// causeToolTimeout (that would blame the tool) and not causeIdleStall (that
// would blame the provider for silence it didn't cause).
func TestStreamWatchdog_HardCapRespectedWithToolInFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not be relevant
	const tick = 10 * time.Millisecond
	const hardCap = 200 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — must not fire before hardCap

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, false, hardCap, toolMaxDuration, 0, nil)

	// A tool starts and stays in flight — never finishes — while the hard
	// cap elapses. The watchdog must fire purely from hardCap, not from the
	// toolMaxDuration backstop.
	wd.toolStarted()

	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog should have fired at hard cap despite a tool being in flight")
	}

	assert.Equal(t, int32(1), fired.Load(), "watchdog must fire at hard cap")
	assert.True(t, wd.stalled.Load())
	assert.Equal(t, causeHardCap, watchdogCause(firedCause.Load()),
		"the hard-cap fire while a tool is in flight must report causeHardCap — this is a wall-clock turn limit, not a stuck tool and not provider idle")
}

// TestStreamWatchdog_HardCapWhileToolInFlightDistinctFromIdleStall proves
// the two previously-indistinguishable "not a tool timeout" cases — a
// hard-cap fire that happens to catch a tool in flight, and a genuine
// provider idle-stall with no tool in flight — now report DIFFERENT
// watchdogCause values from the same onFire callback shape. Before task
// #223 both cases passed toolTimeout=false and were fully indistinguishable
// to the caller; this is the direct regression test for that gap.
func TestStreamWatchdog_HardCapWhileToolInFlightDistinctFromIdleStall(t *testing.T) {
	t.Parallel()

	const tick = 10 * time.Millisecond

	// Case 1: hard cap fires while a tool is in flight.
	hardCapCtx, hardCapCancel := context.WithCancel(t.Context())
	defer hardCapCancel()
	const hardCap = 150 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — never-freeze backstop must not preempt hardCap
	var hardCapCause atomic.Int32
	hardCapWd := startStreamWatchdog(hardCapCtx, hardCapCancel, 5*time.Second, tick,
		func(_ time.Duration, cause watchdogCause) {
			hardCapCause.Store(int32(cause))
		}, false, hardCap, toolMaxDuration, 0, nil)
	hardCapWd.toolStarted()

	// Case 2: genuine idle stall, no tool ever in flight, no hard cap
	// configured.
	idleCtx, idleCancel := context.WithCancel(t.Context())
	defer idleCancel()
	const idleTimeout = 60 * time.Millisecond
	var idleCause atomic.Int32
	idleWd := startStreamWatchdog(idleCtx, idleCancel, idleTimeout, tick,
		func(_ time.Duration, cause watchdogCause) {
			idleCause.Store(int32(cause))
		}, false, 0, 0, 0, nil)

	select {
	case <-hardCapWd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("hard-cap watchdog never fired")
	}
	select {
	case <-idleWd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("idle watchdog never fired")
	}

	gotHardCap := watchdogCause(hardCapCause.Load())
	gotIdle := watchdogCause(idleCause.Load())

	assert.Equal(t, causeHardCap, gotHardCap, "hard-cap-with-tool-in-flight must report causeHardCap")
	assert.Equal(t, causeIdleStall, gotIdle, "genuine provider idle-stall must report causeIdleStall")
	assert.NotEqual(t, gotHardCap, gotIdle,
		"a hard-cap fire while a tool is in flight and a genuine idle-stall must be distinguishable, not collapse to the same cause")
}
