package tools

import (
	"fmt"

	"charm.land/fantasy"
)

// StallDetachedJobController lets the agent package serve its stall-detached
// synchronous tool jobs (turn-stall policy layer, chunk 3) through
// job_output/job_kill. Installed once by the agent package's init; nil
// disables the hooks entirely.
type StallDetachedJobController interface {
	// StallDetachOutput reports a detached job's output; ok=false means the
	// id is not a detached job.
	StallDetachOutput(sessionID, jobID string, cursor int64) (text string, done bool, next int64, ok bool)
	// StallDetachKill stops a detached job; ok=false means the id is not a
	// detached job.
	StallDetachKill(sessionID, jobID string) (text string, ok bool)
}

// StallDetachedJobs is the pluggable controller (nil by default).
var StallDetachedJobs StallDetachedJobController

// stallDetachOutputResponse serves job_output for a stall-detached job;
// handled=false falls through to the shell/ledger paths.
func stallDetachOutputResponse(sessionID string, params JobOutputParams) (fantasy.ToolResponse, bool) {
	if StallDetachedJobs == nil || params.JobID == "" {
		return fantasy.ToolResponse{}, false
	}
	text, done, next, ok := StallDetachedJobs.StallDetachOutput(sessionID, params.JobID, params.Cursor)
	if !ok {
		return fantasy.ToolResponse{}, false
	}
	status := "running"
	if done {
		status = "completed"
	}
	metadata := JobOutputResponseMetadata{JobID: params.JobID, Done: done, NextCursor: next}
	result := fmt.Sprintf("Status: %s\n\n%s", status, text)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), true
}

// stallDetachKillResponse serves job_kill for a stall-detached job;
// handled=false falls through to the shell/ledger paths.
func stallDetachKillResponse(sessionID, jobID string) (fantasy.ToolResponse, bool) {
	if StallDetachedJobs == nil || jobID == "" {
		return fantasy.ToolResponse{}, false
	}
	text, ok := StallDetachedJobs.StallDetachKill(sessionID, jobID)
	if !ok {
		return fantasy.ToolResponse{}, false
	}
	metadata := JobKillResponseMetadata{JobID: jobID}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), metadata), true
}
