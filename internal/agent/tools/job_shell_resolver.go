package tools

import (
	"errors"
	"fmt"
	"strings"
)

// JobShellResolver resolves a model-visible async job id (the id returned in
// "Async bash job <id> started", equal to the tool call id -- see
// asyncToolMetadata.JobID in internal/agent/async_tool.go) to the background
// shell id backing it, scoped to the owning session. Implemented by the
// agent package's work ledger and injected here so this package never
// imports internal/agent -- see coordinator_tools.go's wiring.
type JobShellResolver interface {
	// ResolveJobShellID resolves jobID, owned by sessionID, to a background
	// shell id. The returned error's message is safe to show the model
	// directly (job_output/job_kill return it via NewTextErrorResponse). A
	// run_command or delegation job_id returns *RunCommandJobError /
	// *DelegationJobError instead of a plain error -- see those types.
	ResolveJobShellID(sessionID, jobID string) (string, error)
	// MarkJobStopped records that jobID (owned by sessionID) is being
	// stopped by an explicit job_kill call, BEFORE job_kill actually kills
	// the underlying shell (task #1023 §2.2), and returns job_kill's own
	// final answer text (task #1063) plus a verdict (B11):
	//   - JobStopStopped: this call's own transition won; text is the real
	//     output snapshot worded "stopped (job_kill)" and job_kill must now
	//     kill the shell.
	//   - JobStopAlreadyTerminal: the ledger row already reached a terminal
	//     state via another cause (natural finish, timeout, Stop) -- text is
	//     worded from that COMMITTED row, and job_kill must return it as is
	//     WITHOUT touching the shell manager.
	//   - JobStopNotFound: jobID is not a live, ledger-tracked job (unknown,
	//     already delivered, or a concurrent job_kill already claimed it) --
	//     text is empty and job_kill refuses with the idempotent "not found
	//     ... or already stopped" answer (contract §1.5), again without
	//     touching the shell manager.
	// claimID is the claim of the row a JobStopStopped call stopped (empty
	// otherwise, and for a job that has no durable row): job_kill puts it in
	// its result metadata so the ledger can fuse that result onto exactly that
	// row, or re-pend exactly that row if the result is not recorded (R2A-8).
	MarkJobStopped(sessionID, jobID string) (text, claimID string, verdict JobStopVerdict)
}

// JobStopVerdict is MarkJobStopped's three-way outcome.
type JobStopVerdict int

const (
	// JobStopNotFound: no live tracked job to stop; refuse.
	JobStopNotFound JobStopVerdict = iota
	// JobStopStopped: this call won the stop; kill the shell, answer with text.
	JobStopStopped
	// JobStopAlreadyTerminal: the row is already terminal via another cause;
	// answer with text, do not kill.
	JobStopAlreadyTerminal
)

// RunCommandController lets job_kill/job_output act on a run_command job,
// which has no background shell for BackgroundShellManager to operate on
// (task #1023 §3). Implemented by the work ledger; nil disables run_command
// job control -- job_kill/job_output then just surface RunCommandJobError's
// text as-is (the pre-#1023 behavior).
type RunCommandController interface {
	// RunCommandOutput returns jobID's live output from cursor (§2.1),
	// whether the job has finished, and the next cursor to pass on the next
	// call. err is model-safe (not found / not owned / already delivered).
	RunCommandOutput(sessionID, jobID string, cursor int64) (data string, done bool, nextCursor int64, err error)
	// StopRunCommandJob marks jobID stopped-on-request (§2.2) and cancels
	// its executor context -- the only way to stop a run_command job. On
	// success, text is job_kill's own final answer (task #1063): the real
	// output snapshot taken before the kill, worded like
	// JobShellResolver.MarkJobStopped's. err is model-safe; a second call on
	// an already-stopped job returns the same "not found" shape as any other
	// refusal (contract §1.5's idempotency rule). claimID is as for
	// MarkJobStopped.
	StopRunCommandJob(sessionID, jobID string) (text, claimID string, err error)
}

// RunCommandJobError is returned by JobShellResolver.ResolveJobShellID when
// jobID addresses a run_command job: run_command has no background shell, so
// job_kill/job_output route it through RunCommandController instead of
// BackgroundShellManager. A typed error (not a string match) so that routing
// decision does not depend on exact wording.
type RunCommandJobError struct{ JobID string }

func (e *RunCommandJobError) Error() string {
	return fmt.Sprintf("job %s is a run_command job, not a background shell", e.JobID)
}

// DelegationJobError is returned when jobID addresses a delegation (agent/
// agentic_fetch) job. job_kill/job_output refuse these directly and point at
// the dedicated sub-agent control tools (task #1024) -- see wake-tools-
// contract.md §4.
type DelegationJobError struct{ JobID, ChildSessionID string }

func (e *DelegationJobError) Error() string {
	return fmt.Sprintf(
		"job %s is a sub-agent delegation (child session %s), not a command -- use stop_agent to stop it or inspect_agent to check its status; its result will arrive as a session message when the sub-agent finishes",
		e.JobID, e.ChildSessionID,
	)
}

// errJobShellIDRequired is the shared validation error for job_kill and
// job_output: exactly one of job_id/shell_id must be given.
var errJobShellIDRequired = errors.New("provide exactly one of job_id or shell_id")

// resolveShellID validates the job_id/shell_id pair shared by job_kill and
// job_output and returns the concrete shell id to operate on. shellID given
// directly is used as-is (backward compatible with pre-job_id callers);
// jobID is resolved through resolver, scoped to sessionID.
func resolveShellID(resolver JobShellResolver, sessionID, jobID, shellID string) (string, error) {
	jobID = strings.TrimSpace(jobID)
	shellID = strings.TrimSpace(shellID)
	if (jobID == "") == (shellID == "") { // both empty or both set
		return "", errJobShellIDRequired
	}
	if shellID != "" {
		return shellID, nil
	}
	if resolver == nil {
		return "", fmt.Errorf("job %s not found (job_id resolution is unavailable)", jobID)
	}
	return resolver.ResolveJobShellID(sessionID, jobID)
}

// BGShellStopper is the optional second capability of the resolver job_kill
// receives: it stops a background shell that has a durable async_jobs row of
// kind bg_shell but no in-memory ledger job (a shell started by a SYNC bash
// call, R-BG-1), and so is invisible to MarkJobStopped. A resolver that does
// not implement it leaves job_kill on its older path.
type BGShellStopper interface {
	// StopBackgroundShellRow records the row's terminal "cancelled" transition
	// BEFORE job_kill kills the process, with the same verdicts as
	// MarkJobStopped: JobStopStopped (the row is cancelled, text is the output
	// snapshot, kill the shell), JobStopAlreadyTerminal (answer with text, do
	// not kill) and JobStopNotFound (shellID has no running bg_shell row: take
	// the older path).
	StopBackgroundShellRow(sessionID, shellID string) (text, claimID string, verdict JobStopVerdict)
}
