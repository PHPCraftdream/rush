package app

import (
	"context"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// Stage 5a: the wake-schedule side of the CLI lifetime (operator decisions,
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md sec.5): an
// ACTIVE once schedule holds the run open (CLIScopeState.OnceWakeOpen ->
// WorkOpen), an ACTIVE loop schedule never does -- when the loop's scope
// closes with nothing else open, the session's loop schedules are cancelled
// so the process cannot be kept alive by, or leave behind, an endless timer.
// A Ctrl-C/--timeout/`sessions cancel` exit is NOT a scope close: the
// schedules stay in the DB and fire when any host of that DB runs.

// cancelLoopSchedulesAtClose cancels the session's ACTIVE loop schedules at
// the ordinary scope-closed exit: one stderr line, one envelope warning
// (loopTotals.extraWarnings, so the flush carries it). Best effort on a
// detached context: a cancel failure is logged and never turns a clean end
// into an error. Once schedules are never touched (they hold the process,
// so a scope close cannot be happening while one is open) and other
// sessions' schedules are out of reach (the store API is owner-checked).
//
// It runs at EVERY scope close, not once per run: a reminder or reviewer turn
// that follows a close can create a loop schedule of its own (C9-3), and the
// next close must cancel that one too. A close with nothing to cancel is
// silent.
func (l *cliLoop) cancelLoopSchedulesAtClose() {
	canceler, ok := l.source.(agent.LoopWakeCanceler)
	if !ok || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	n, err := canceler.CancelLoopWakeSchedules(ctx, l.sessionID)
	if err != nil {
		fmt.Fprintf(l.errOut(), "rush run: session %q: cancelling the loop schedules at run end failed: %v\n", l.sessionID, err)
		return
	}
	if n == 0 {
		return
	}
	text := fmt.Sprintf("%d loop schedule(s) cancelled at run end", n)
	fmt.Fprintf(l.errOut(), "rush run: session %q: %s\n", l.sessionID, text)
	l.tot.extraWarnings = append(l.tot.extraWarnings, text)
}

// describeOnceWake renders the schedule clause of the wait heartbeat:
// what fires, when, in how long -- the same concrete-naming rule as the
// running-work clause (ASYNC-10).
func (l *cliLoop) describeOnceWake() string {
	s := l.lastScope
	if !s.OnceWakeOpen {
		return ""
	}
	in := "now"
	if d := time.Until(s.OnceWakeAt); d > 0 {
		in = "in " + d.Round(time.Second).String()
	}
	return fmt.Sprintf("wake schedule %s at %s (%s)", s.OnceWakeID,
		s.OnceWakeAt.Local().Format("15:04:05"), in)
}
