package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
)

// ErrRunDefaultCap remains compatible with default-cap agent diagnosis.
var ErrRunDefaultCap = fmt.Errorf("run default wall-clock cap reached: %w", context.DeadlineExceeded)

// RunTimeoutCause identifies the run's own wall-clock deadline.
type RunTimeoutCause struct {
	Duration   time.Duration
	Source     string
	DefaultCap bool
}

func (e *RunTimeoutCause) Error() string {
	hint := ""
	if e.DefaultCap {
		hint = "; pass --timeout to change the cap"
	}
	return fmt.Sprintf("run timeout %s exceeded (source: %s%s)", e.Duration, e.Source, hint)
}

func (e *RunTimeoutCause) Unwrap() error { return context.DeadlineExceeded }

func (e *RunTimeoutCause) Is(target error) bool {
	return e.DefaultCap && target == ErrRunDefaultCap
}

// runTimeoutFinishText is the finish message of a turn cut off by the run's
// deadline; cause is context.Cause of the turn's context.
func runTimeoutFinishText(cause error, sessionID string) (title, details string) {
	what := "The run's --timeout deadline expired"
	if errors.Is(cause, ErrRunDefaultCap) {
		what = "The run's default wall-clock cap expired (no --timeout was set; RUSH_RUN_DEFAULT_HARD_TIMEOUT sets it)"
	}
	return "Run timeout exceeded", fmt.Sprintf(
		"%s while this turn was still in flight (e.g. a long tool call or sub-agent delegation).\n\n%s",
		what, watchdog.WatchdogResumeGuidance(sessionID, "--timeout"),
	)
}
