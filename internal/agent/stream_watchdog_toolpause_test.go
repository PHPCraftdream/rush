// Tests for the toolsInFlight pause: the idle timer freezes while tools run
// and the pause is reference-counted; the never-freeze toolMaxDuration
// backstop bounds it, with the cleanup-grace window delaying that backstop
// so a nested watchdog can unwind first.

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestStreamWatchdog_PausedDuringToolExecution verifies the idle timer is
// frozen while a tool is executing — a long `cargo`/compile run is not a
// provider stall and must not be force-cancelled.
func TestStreamWatchdog_PausedDuringToolExecution(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)
	// A tool starts and runs WAY past idleTimeout with zero provider
	// activity — the watchdog must NOT fire.
	wd.toolStarted()
	time.Sleep(idle * 4)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool is executing, even past idleTimeout")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and must
	// fire after the idle window.
	wd.toolFinished()
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire after the tool finished and the stream went idle")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_PauseCountsParallelTools verifies the pause is
// reference-counted: finishing one of several in-flight tools must keep the
// watchdog paused until ALL of them complete.
func TestStreamWatchdog_PauseCountsParallelTools(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 50 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)
	// Two parallel tool calls in flight; finishing ONE must keep the
	// watchdog paused (counter still > 0).
	wd.toolStarted()
	wd.toolStarted()
	wd.toolFinished()
	time.Sleep(idle * 3)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must stay paused while any tool is still in flight")
	assert.False(t, wd.stalled.Load())
}

// TestStreamWatchdog_ToolPauseBoundedByCap verifies the never-freeze
// backstop: when toolMaxDuration > 0 and a tool stays in flight past that
// cap, the watchdog fires with toolTimeout==true instead of pausing
// forever. This is what keeps a stuck tool (hung MCP tool, blocking
// job_output --wait, or a sub-agent delegation via the "agent" tool that
// never returns) from freezing the whole agent turn. One cap applies to
// every tool — there is no separate, larger cap for delegations anymore
// (see toolExecutionMaxDefault's doc in agent.go for why that split was
// removed), so this test doubles as coverage for the delegation case too:
// toolStarted/toolFinished no longer take any argument distinguishing tool
// kinds, so there is no way to even express a different cap at the call
// site.
func TestStreamWatchdog_ToolPauseBoundedByCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	const idle = 5 * time.Second // large — idle path must NOT fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration, 0, nil)

	// A tool starts and runs past toolMaxDuration with zero provider
	// activity. The watchdog must fire with cause=causeToolTimeout.
	wd.toolStarted()
	select {
	case <-wd.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog never fired after toolMaxDuration")
	}

	assert.Equal(t, int32(1), fired.Load(), "onFire should fire exactly once")
	assert.Equal(t, causeToolTimeout, watchdogCause(firedCause.Load()), "cause must be causeToolTimeout when the cap is exceeded")
	assert.True(t, wd.stalled.Load(), "stalled flag must be true after fire")
	assert.Error(t, ctx.Err(), "ctx must be cancelled by the watchdog")
	assert.GreaterOrEqual(t, time.Duration(firedElapsed.Load()), toolMaxDuration,
		"elapsed passed to onFire must be >= toolMaxDuration")
}

// TestStreamWatchdog_ToolPauseUnderCapDoesNotFire verifies that a tool
// running UNDER the cap does not trip the backstop, and that after the
// tool finishes the watchdog still fires normally on idle.
func TestStreamWatchdog_ToolPauseUnderCapDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — well above the tool runtime

	var fired atomic.Int32
	var firedCause atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(_ time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
	}, false, 0, toolMaxDuration, 0, nil)
	defer func() {
		cancel()
		<-wd.done
	}()

	// Tool runs for a few idle periods — well under the cap. The
	// watchdog must NOT fire.
	wd.toolStarted()
	time.Sleep(idle * 3)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire while a tool runs under the cap")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Tool finishes; with no further activity the watchdog resumes and
	// must fire on idle afterwards (cause==causeIdleStall).
	wd.toolFinished()
	select {
	case <-wd.done:
	case <-time.After(idle + 300*time.Millisecond):
		t.Fatal("watchdog must fire on idle after the tool finished")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.Equal(t, causeIdleStall, watchdogCause(firedCause.Load()), "the post-tool fire must be an idle fire, not a tool timeout")
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_SequentialBatchProgressResetsCapClock is the regression
// test for a real bug found while live-testing the never-freeze backstop: a
// sub-agent running FOUR individually-fast bash steps (well under the
// configured cap) was force-cancelled anyway, because fantasy fires every
// OnToolCall for a step BEFORE executing any of the tools in it — so a
// "batch" the model issued as several back-to-back tool calls (common for
// faster/smaller models even when explicitly asked to go one at a time) is
// indistinguishable, from the watchdog's counter alone, from true parallel
// execution. Before this fix, toolStartedAt was set once when the FIRST
// tool of the batch started and only reset to 0 once ALL of them had
// finished — so toolMaxDuration bounded the batch's CUMULATIVE wall time,
// not any single tool's runtime, and several fast sequential steps could
// sum past the cap and get killed even though none was ever stuck.
//
// This proves: with toolMaxDuration = 60ms, four tools started together
// (simulating fantasy's upfront OnToolCall batch) finish one at a time
// ~30ms apart (each individual gap safely under the cap, but the batch's
// total span, ~120ms, is well past it) — the watchdog must NOT fire, since
// every gap between consecutive finishes resets the clock.
func TestStreamWatchdog_SequentialBatchProgressResetsCapClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not confound this test
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 600 * time.Millisecond
	// stepGap < toolMaxDuration; 4 steps sum to ~1200ms > toolMaxDuration.
	// Scaled 10x up from an earlier 30ms/60ms version (task #320): the
	// invariant this test needs is "each individual gap stays under the
	// cap", and that margin was only 30ms wide — well inside typical
	// scheduler jitter for a goroutine under -race in a full-package
	// parallel run, where a delayed time.Sleep wakeup could push a single
	// gap over the cap and flake the test on pure scheduling luck, not a
	// real bug. Same 2x ratio, ten times the absolute margin.
	const stepGap = 300 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, 0, nil)

	// fantasy fires OnToolCall for every tool in the step before executing
	// any of them — simulate that: all four "start" near-simultaneously.
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()
	wd.toolStarted()

	// They finish one at a time, ~300ms apart — each gap is safely under
	// the 600ms cap, but the cumulative batch span (~1200ms) is not.
	for i := 0; i < 4; i++ {
		time.Sleep(stepGap)
		wd.toolFinished()
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire for a sequential batch whose individual step gaps stay under the cap, even though the cumulative span exceeds it")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// A genuinely stuck tool must still be caught: after the batch above
	// fully finishes (toolsInFlight back to 0), start one more tool and let
	// it run well past the cap with no further progress — this must fire.
	wd.toolStarted()
	select {
	case <-wd.done:
	case <-time.After(toolMaxDuration + time.Second):
		t.Fatal("watchdog must still fire for a genuinely stuck tool after a healthy sequential batch")
	}
	assert.Equal(t, int32(1), fired.Load())
	assert.True(t, wd.stalled.Load())
}

// TestStreamWatchdog_ToolCleanupGraceDelaysFire is the regression test for
// the parent/child cancellation race: an `agent`-tool delegation runs a
// nested Run()/runTurn() with its OWN stream watchdog, started strictly
// LATER than the parent's (the parent starts timing from OnToolCall, before
// the child's turn has even begun executing). Both share the same
// toolMaxDuration, so without a grace buffer the parent's head start means
// it always reaches the cap first and force-cancels the whole delegation
// before the child's own watchdog gets a chance to fire on ITS cap and
// unwind cleanly. This test proves the fix at the primitive level: with
// toolCleanupGrace > 0, the watchdog must NOT fire at toolMaxDuration — it
// must only fire once toolMaxDuration+toolCleanupGrace has elapsed.
func TestStreamWatchdog_ToolCleanupGraceDelaysFire(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond
	const toolCleanupGrace = 120 * time.Millisecond

	var fired atomic.Int32
	var firedCause atomic.Int32
	var firedElapsed atomic.Int64
	start := time.Now()
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(elapsed time.Duration, cause watchdogCause) {
		fired.Add(1)
		firedCause.Store(int32(cause))
		firedElapsed.Store(int64(elapsed))
	}, false, 0, toolMaxDuration, toolCleanupGrace, nil)

	wd.toolStarted()

	// Wait until just past toolMaxDuration alone (well before the grace
	// window closes) — the watchdog must NOT have fired yet.
	time.Sleep(toolMaxDuration + 20*time.Millisecond)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire at bare toolMaxDuration when toolCleanupGrace > 0 — "+
			"the grace exists precisely so a nested child watchdog gets a chance to fire first")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())

	// Now wait for the fire past toolMaxDuration+toolCleanupGrace.
	select {
	case <-wd.done:
	case <-time.After(time.Second):
		t.Fatal("watchdog never fired after toolMaxDuration+toolCleanupGrace")
	}
	elapsedWall := time.Since(start)

	assert.Equal(t, int32(1), fired.Load(), "onFire should fire exactly once")
	assert.Equal(t, causeToolTimeout, watchdogCause(firedCause.Load()), "cause must be causeToolTimeout when the cap is exceeded")
	assert.True(t, wd.stalled.Load())
	assert.Error(t, ctx.Err())
	assert.GreaterOrEqual(t, time.Duration(firedElapsed.Load()), toolMaxDuration+toolCleanupGrace,
		"elapsed passed to onFire must be >= toolMaxDuration+toolCleanupGrace")
	assert.GreaterOrEqual(t, elapsedWall, toolMaxDuration+toolCleanupGrace,
		"wall-clock time to fire must respect the full grace window")
}

// TestStreamWatchdog_ToolFinishesWithinGraceNeverFires verifies the healthy
// counterpart of the race: if the tool (e.g. a sub-agent delegation)
// finishes on its own — reacting to ITS OWN toolMaxDuration and unwinding
// cleanly — at some point AFTER the parent's bare toolMaxDuration but BEFORE
// toolMaxDuration+toolCleanupGrace elapses, the parent watchdog must never
// fire at all. This is the scenario toolCleanupGrace exists to make
// possible: the child wins the race to finish before the parent's grace
// period runs out.
func TestStreamWatchdog_ToolFinishesWithinGraceNeverFires(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — idle path must not fire
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 60 * time.Millisecond
	const toolCleanupGrace = 150 * time.Millisecond

	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, toolCleanupGrace, nil)
	defer func() {
		cancel()
		<-wd.done
	}()

	wd.toolStarted()

	// Simulate the child unwinding cleanly at a point past bare
	// toolMaxDuration but comfortably inside the grace window.
	time.Sleep(toolMaxDuration + 40*time.Millisecond)
	wd.toolFinished()

	// Give the watchdog a couple more ticks to prove it did NOT fire despite
	// having crossed bare toolMaxDuration while the tool was in flight.
	time.Sleep(3 * tick)
	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must not fire when the tool finishes within the grace window, "+
			"even though it ran past bare toolMaxDuration")
	assert.False(t, wd.stalled.Load())
	assert.NoError(t, ctx.Err())
}

// TestStreamWatchdog_NoTimerDrivenActivityWhileToolInFlight is the
// regression test for task #300, and deliberately asserts the OPPOSITE of
// what task #222's test used to.
//
// #222 made the watchdog call recordActivity once per tick for as long as
// any tool was in flight, so a long synchronous tool (which produces no
// stream callbacks) would not starve SessionLock's activity-gated
// heartbeat. The cost was that the heartbeat then reported "alive" purely
// because a tool call was OPEN: a wedged tool looked exactly like a
// working one for its entire cap. That was observed in the wild as a
// session showing a fresh heartbeat for 38 minutes while its sub-agent was
// stuck on a trivial command, with sessions locks/why/list all calling it
// healthy.
//
// The heartbeat must now reflect only REAL progress. #222's original
// concern is covered without a timer: withActivityNotify (agent.go)
// composes each session's activity callback with its ancestors', so a
// delegated sub-agent's genuine stream callbacks walk back up and touch
// every ancestor session's lock. Nothing else may synthesise activity.
func TestStreamWatchdog_NoTimerDrivenActivityWhileToolInFlight(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 5 * time.Second // large — the idle path must not confound this
	const tick = 10 * time.Millisecond
	const toolMaxDuration = 5 * time.Second // generous — the tool must not be cut off

	var recordCalls atomic.Int32
	var fired atomic.Int32
	wd := startStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdogCause) {
		fired.Add(1)
	}, false, 0, toolMaxDuration, 0, func() {
		recordCalls.Add(1)
	})
	defer func() {
		cancel()
		<-wd.done
	}()

	// A tool stays in flight for many ticks while producing NOTHING — the
	// exact shape of a hung tool call.
	wd.toolStarted()
	time.Sleep(15 * tick)

	assert.Equal(t, int32(0), fired.Load(), "watchdog must not fire while the tool is under its cap")
	assert.Equal(t, int32(0), recordCalls.Load(),
		"a tool merely being OPEN must never synthesise activity: the timer must not touch the heartbeat, "+
			"otherwise a wedged tool is indistinguishable from a working one for its whole cap (task #300)")

	// A real stream callback — genuine progress — still records activity.
	wd.bump()
	assert.Greater(t, recordCalls.Load(), int32(0),
		"real progress (a stream callback) must still record activity — this is the ONLY thing that may")

	wd.toolFinished()
}
