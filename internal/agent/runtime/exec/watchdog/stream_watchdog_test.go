package watchdog_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	_ "github.com/joho/godotenv/autoload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}

// All tests use short timeouts (tens of ms) to keep the suite fast. The
// real production constants (streamIdleTimeout = 3min, tick = 30s) are not
// exercised here — that's a property of the integration, not of this unit.

func TestStreamWatchdog_BumpKeepsItAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())

		// The synctest bubble supplies virtual time, so this test has no scheduler
		// or wall-clock threshold race.
		const idle = 240 * time.Millisecond
		const tick = 30 * time.Millisecond

		var fired atomic.Int32
		wd := watchdog.StartStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdog.Cause) {
			fired.Add(1)
		}, false, 0, 0, 0, nil)
		// Bump every 60ms for 1.2s, well past idleTimeout.
		// Watchdog must NOT fire.
		for i := 0; i < 20; i++ {
			time.Sleep(60 * time.Millisecond)
			wd.Bump()
		}

		assert.Equal(t, int32(0), fired.Load(),
			"watchdog must not fire while bump() is called more often than idleTimeout")
		assert.False(t, wd.Stalled.Load(), "stalled flag must stay false")
		assert.NoError(t, ctx.Err(), "ctx must not be cancelled by the watchdog")
		cancel()
		<-wd.Done
	})
}

func TestStreamWatchdog_FiresOnNoActivity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 60 * time.Millisecond
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	var firedIdle atomic.Int64
	var firedCause atomic.Int32
	wd := watchdog.StartStreamWatchdog(ctx, cancel, idle, tick, func(observedIdle time.Duration, cause watchdog.Cause) {
		fired.Add(1)
		firedIdle.Store(int64(observedIdle))
		firedCause.Store(int32(cause))
	}, false, 0, 0, 0, nil)

	// Wait long enough for the watchdog to fire on its own.
	select {
	case <-wd.Done:
		// Good — watchdog exited (it fires THEN exits).
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog never fired after idle period")
	}

	assert.Equal(t, int32(1), fired.Load(), "onFire should be called exactly once")
	assert.True(t, wd.Stalled.Load(), "stalled flag must be true after fire")
	assert.Error(t, ctx.Err(), "ctx must be cancelled by the watchdog")
	assert.GreaterOrEqual(t, time.Duration(firedIdle.Load()), idle,
		"observed idle passed to onFire must be >= idleTimeout")
	assert.Equal(t, watchdog.CauseIdleStall, watchdog.Cause(firedCause.Load()),
		"a genuine provider idle-stall must report watchdog.CauseIdleStall")
}

func TestStreamWatchdog_ExitsCleanlyOnCtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	const idle = 5 * time.Second // very long — must not fire
	const tick = 10 * time.Millisecond

	var fired atomic.Int32
	wd := watchdog.StartStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdog.Cause) {
		fired.Add(1)
	}, false, 0, 0, 0, nil)

	// Cancel ctx externally — watchdog must exit promptly without firing.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-wd.Done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("watchdog did not exit after external ctx cancel")
	}

	assert.Equal(t, int32(0), fired.Load(),
		"watchdog must NOT fire when ctx is cancelled externally — that's the user/cooperative path")
	assert.False(t, wd.Stalled.Load(),
		"stalled flag stays false because the cancel was NOT the watchdog's doing")
}

func TestStreamWatchdog_BumpAfterFireIsHarmless(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const idle = 30 * time.Millisecond
	const tick = 5 * time.Millisecond

	wd := watchdog.StartStreamWatchdog(ctx, cancel, idle, tick, func(time.Duration, watchdog.Cause) {}, false, 0, 0, 0, nil)

	// Let it fire.
	<-wd.Done
	require.True(t, wd.Stalled.Load())

	// Calling bump() after the goroutine has exited is a no-op — it
	// just stores into an atomic that nobody reads anymore. Must not
	// panic or deadlock.
	require.NotPanics(t, func() {
		wd.Bump()
		wd.Bump()
	})
}
