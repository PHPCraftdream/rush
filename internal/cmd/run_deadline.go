package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// hardKillGrace is how long a `rush run` may take to exit after its deadline
// before the process is force-killed.
const hardKillGrace = 60 * time.Second

// installRunDeadline arms the wall-clock guards of one `rush run` and returns
// the context the run must use plus a stop func (call it on every return).
//
// The deadline is timeoutDur (--timeout) or, when that is not set, defaultCap
// (the default cap, RUSH_RUN_DEFAULT_HARD_TIMEOUT). Both are the SAME graceful
// context deadline: a loop that is waiting -- phase 4 holds the run open for
// jobs and reaction chains -- ends through its normal exit (envelope,
// --on-finish, App.Shutdown) instead of being killed; the command renderer
// prints the timeout notice. Only a process still alive `grace` past the
// deadline (a deadlock, a read that ignores ctx) is force-killed through exit
// (124): that is the zombie backstop, and it holds the session lock no longer
// than the deadline plus the grace.
//
// The cap is not re-armed on progress: nothing observable from outside the loop
// tells a healthy wait from a wedged process, so a wait longer than the cap
// needs an explicit --timeout.
func installRunDeadline(ctx context.Context, timeoutDur, defaultCap, grace time.Duration, stderr io.Writer, exit func(int)) (context.Context, func()) {
	deadline, isCap := timeoutDur, false
	if deadline <= 0 {
		deadline, isCap = defaultCap, true
	}
	source := "--timeout"
	if isCap {
		source = "default cap (RUSH_RUN_DEFAULT_HARD_TIMEOUT; no --timeout set)"
	}
	ctx, cancel := context.WithTimeoutCause(ctx, deadline, &agent.RunTimeoutCause{
		Duration: deadline, Source: source, DefaultCap: isCap,
	})
	timers := make([]*time.Timer, 0, 1)
	timers = append(timers, time.AfterFunc(deadline+grace, func() {
		if cause, ok := context.Cause(ctx).(*agent.RunTimeoutCause); !ok || cause == nil {
			return
		}
		if isCap {
			fmt.Fprintf(stderr,
				"rush: run exceeded its default cap of %s + %s grace (no --timeout set; override via RUSH_RUN_DEFAULT_HARD_TIMEOUT) without exiting — force-killing\n",
				deadline, grace)
		} else {
			fmt.Fprintf(stderr,
				"rush: run exceeded %s (timeout %s + %s grace) without exiting — force-killing\n",
				deadline+grace, deadline, grace)
		}
		exit(124)
	}))
	return ctx, func() {
		for _, t := range timers {
			t.Stop()
		}
		cancel()
	}
}
