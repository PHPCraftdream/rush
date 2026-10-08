package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// RunTimeoutError carries the owned deadline and exact continuation command.
type RunTimeoutError struct {
	Cause         *agent.RunTimeoutCause
	ResumeCommand string
}

func (e *RunTimeoutError) Error() string { return e.Cause.Error() }
func (e *RunTimeoutError) Unwrap() error { return e.Cause }

func runResumeRole(role string) string {
	if role != "" {
		return role
	}
	return "smart"
}

func classifyRunTimeout(ctx context.Context, final *RunResult, err error, sessionID, role string) (*RunResult, error) {
	if ctx.Err() == nil {
		return final, err
	}
	var cause *agent.RunTimeoutCause
	if !errors.As(context.Cause(ctx), &cause) {
		return final, err
	}
	resume := ""
	if sessionID != "" {
		resume = fmt.Sprintf("rush run --role %s --session %s", runResumeRole(role), sessionID)
	}
	timeoutErr := &RunTimeoutError{Cause: cause, ResumeCommand: resume}
	if final == nil {
		final = &RunResult{SessionID: sessionID, ToolCalls: []ToolCallStat{}}
	}
	final.ExitReason = "timeout"
	final.Error = timeoutErr.Error()
	final.ResumeCommand = resume
	return final, timeoutErr
}
