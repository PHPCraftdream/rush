package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStreamWatchdog_DisarmSuppressesTheHardCap covers task #548.
//
// runTurn spends up to titleJoinGrace waiting for a title after its real
// work is done, with no bump() calls. The idle timer tolerates that
// (streamIdleTimeoutDefault is ten minutes), but --timeout-hard-cap is
// absolute from turn start, so a turn finishing just inside the cap could be
// pushed over it by the wait alone: a stall dump and a warning for a turn
// that had already succeeded, plus a cancel that killed the title being
// waited for.
//
// Revert-check: remove the disarmed check from the watchdog's tick handler
// and this fails -- onFire runs and reports causeHardCap.
func TestStreamWatchdog_DisarmSuppressesTheHardCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fires atomic.Int64
	var lastCause atomic.Int32
	wd := startStreamWatchdog(
		ctx, cancel,
		time.Hour,          // idleTimeout: far away, so only the hard cap can fire
		5*time.Millisecond, // tick
		func(elapsed time.Duration, cause watchdogCause) {
			fires.Add(1)
			lastCause.Store(int32(cause))
		},
		false,               // extendsOnProgress
		40*time.Millisecond, // hardCap: deliberately imminent
		time.Hour,           // toolMaxDuration
		0,                   // toolCleanupGrace
		func() {},           // recordActivity
	)

	wd.disarm()

	// Well past the hard cap. Nothing may fire.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int64(0), fires.Load(),
		"the watchdog fired after being disarmed (cause=%d): the title join happens in exactly this window, on a turn whose work already succeeded", lastCause.Load())

	cancel()
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("disarming must not stop the goroutine from exiting on ctx -- runTurn's deferred <-wd.done would hang forever")
	}
}

// TestStreamWatchdog_ArmedStillFiresTheHardCap is the control. Disarm must
// suppress the watchdog only when asked; an untouched one still has to catch
// a turn that runs past its cap, or the previous test would pass against a
// watchdog that never fires at all.
func TestStreamWatchdog_ArmedStillFiresTheHardCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := make(chan watchdogCause, 1)
	wd := startStreamWatchdog(
		ctx, cancel,
		time.Hour,
		5*time.Millisecond,
		func(elapsed time.Duration, cause watchdogCause) {
			select {
			case fired <- cause:
			default:
			}
		},
		false,
		40*time.Millisecond,
		time.Hour,
		0,
		func() {},
	)

	select {
	case cause := <-fired:
		require.Equal(t, causeHardCap, cause, "an armed watchdog past its hard cap must fire causeHardCap")
	case <-time.After(3 * time.Second):
		t.Fatal("an armed watchdog never fired past its hard cap -- the disarm test above would prove nothing")
	}

	cancel()
	select {
	case <-wd.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog goroutine did not exit")
	}
}
