package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// RunTimeoutError carries the owned deadline and exact continuation command.
// CommittedResult reports that the invocation had already finished cleanly
// (result envelope, no error) before the deadline was noticed: the run exits
// timed out, but the envelope still carries that committed answer -- the
// timeout reports how the RUN ended, it never erases a committed turn's
// outcome (see cliLoop.turnCanceled, which adopts it).
type RunTimeoutError struct {
	Cause           *agent.RunTimeoutCause
	ResumeCommand   string
	CommittedResult bool
}

func (e *RunTimeoutError) Error() string { return e.Cause.Error() }
func (e *RunTimeoutError) Unwrap() error { return e.Cause }

func runResumeRole(role string) string {
	if role != "" {
		return role
	}
	return "smart"
}

// classifyRunTimeout maps an owned run deadline onto the timeout exit: the
// envelope gains the timeout reason/error/resume metadata and the caller gets
// a *RunTimeoutError. A clean invocation outcome (err == nil with an envelope)
// keeps its answer in the envelope and is flagged CommittedResult on the
// error, so the loop can still adopt it; failed or envelope-less turns are
// never marked committed.
func classifyRunTimeout(ctx context.Context, final *RunResult, err error, sessionID, role string) (*RunResult, error) {
	if ctx.Err() == nil {
		return final, err
	}
	resume := ""
	if sessionID != "" {
		resume = fmt.Sprintf("rush run --role %s --session %s", runResumeRole(role), sessionID)
	}
	// Idempotent: several defers classify (the loop exit, both ExecuteRun
	// defers). A repeat keeps the error, and so its CommittedResult, but
	// re-asserts the envelope: the loop's cancel exit may have overwritten
	// the reason with "canceled" in between.
	var timeoutErr *RunTimeoutError
	if errors.As(err, &timeoutErr) {
		if timeoutErr.ResumeCommand == "" {
			timeoutErr.ResumeCommand = resume
		}
	} else {
		var cause *agent.RunTimeoutCause
		if !errors.As(context.Cause(ctx), &cause) {
			return final, err
		}
		committed := err == nil && final != nil
		timeoutErr = &RunTimeoutError{Cause: cause, ResumeCommand: resume, CommittedResult: committed}
	}
	if final == nil {
		final = &RunResult{SessionID: sessionID, ToolCalls: []ToolCallStat{}}
	}
	final.ExitReason = "timeout"
	final.Error = timeoutErr.Error()
	final.ResumeCommand = timeoutErr.ResumeCommand
	return final, timeoutErr
}

// runTimeoutCommitted reports whether err is the timeout of a run whose turn
// had already committed a clean result before the deadline was noticed; only
// such a timeout may be traded for the committed answer.
func runTimeoutCommitted(err error) bool {
	var timeoutErr *RunTimeoutError
	return errors.As(err, &timeoutErr) && timeoutErr.CommittedResult
}
