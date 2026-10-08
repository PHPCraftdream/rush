package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// WatchdogResumeGuidance returns the orchestrator-facing resume
// instruction for a timeout-caused stop (a tool/sub-agent that ran past
// toolMaxDuration, or a turn that hit its --timeout-hard-cap) — sibling
// to AwaitingAnswerGuidance (question_stop.go) and PeakHoursGuidance
// (peak_hours_stop.go): same "this is not a crash, here is the exact
// command" contract, so an orchestrating agent never has to reconstruct
// --session/--timeout syntax from memory under the pressure of a failed
// run.
//
// timeoutFlag is the flag name to suggest raising — "--timeout" for a
// tool-timeout or run-timeout stop, "--timeout-hard-cap" for a hard-cap
// stop — so the suggested command actually targets the limit that fired.
func WatchdogResumeGuidance(sessionID, timeoutFlag string) string {
	return fmt.Sprintf(
		"This is not a crash — rush is intentionally stopping this turn "+
			"because it ran too long. rush is exiting now; it will not retry "+
			"on its own.\n\n"+
			"If an orchestrating agent is driving this session: resume with a "+
			"larger timeout:\n\n"+
			"  rush run --session %s %s <larger-value> \"continue\"\n\n"+
			"This is a normal continuation, not a retry from scratch — the "+
			"session's context is intact.",
		sessionID, timeoutFlag,
	)
}

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
		what, WatchdogResumeGuidance(sessionID, "--timeout"),
	)
}
