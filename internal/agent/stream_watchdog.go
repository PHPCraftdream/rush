package agent

// Compatibility shim for the stream watchdog, which moved verbatim to
// internal/agent/runtime/exec/watchdog/stream_watchdog.go. This file keeps
// the old lowercase names as aliases/forwards so call sites and white-box
// tests keep compiling unchanged.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
)

// watchdogCause aliases the leaf Cause; see the leaf doc for the history.
type watchdogCause = watchdog.Cause

const (
	causeIdleStall   = watchdog.CauseIdleStall
	causeToolTimeout = watchdog.CauseToolTimeout
	causeHardCap     = watchdog.CauseHardCap
	causeStall       = watchdog.CauseStall
)

type streamWatchdog struct {
	bump         func()
	toolStarted  func()
	toolFinished func()
	stalled      *atomic.Bool
	disarm       func()
	done         <-chan struct{}
	forceStall   func()
}

func startStreamWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	idleTimeout, tick time.Duration,
	onFire func(elapsed time.Duration, cause watchdogCause),
	extendsOnProgress bool,
	hardCap time.Duration,
	toolMaxDuration time.Duration,
	toolCleanupGrace time.Duration,
	recordActivity func(),
) streamWatchdog {
	wd := watchdog.StartStreamWatchdog(ctx, cancel, idleTimeout, tick, onFire, extendsOnProgress, hardCap, toolMaxDuration, toolCleanupGrace, recordActivity)
	return streamWatchdog{
		bump:         wd.Bump,
		toolStarted:  wd.ToolStarted,
		toolFinished: wd.ToolFinished,
		stalled:      wd.Stalled,
		disarm:       wd.Disarm,
		done:         wd.Done,
		forceStall:   wd.ForceStall,
	}
}

func watchdogFinishMessage(cause watchdogCause, toolMaxDuration, hardCap, idleTimeout, stallTimeout time.Duration, provider string) (title, body string) {
	return watchdog.FinishMessage(cause, toolMaxDuration, hardCap, idleTimeout, stallTimeout, provider)
}

func composeWatchdogFinishBody(sessionID string, cause watchdogCause, toolMaxDuration, hardCap, idleTimeout, stallTimeout time.Duration, provider string) string {
	return watchdog.ComposeFinishBody(sessionID, cause, toolMaxDuration, hardCap, idleTimeout, stallTimeout, provider)
}

func watchdogToolResultMessage(cause watchdogCause, toolMaxDuration, hardCap, idleTimeout, stallTimeout time.Duration, provider string) string {
	return watchdog.ToolResultMessage(cause, toolMaxDuration, hardCap, idleTimeout, stallTimeout, provider)
}
