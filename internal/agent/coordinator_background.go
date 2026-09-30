// Background-job completion notices, Phase 4 autonomous idle-resume, and
// busy-state queries. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// filterNonEmpty returns the subset of inputs that are non-empty after
// trimming surrounding whitespace. Used to join stdout/stderr cleanly.
func filterNonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// FormatAsyncCompletion makes a completed job visible to the model and UI.
// The TimedOut branch's wording matches
// docs/plans/2026-09-27-wake-tools-contract.md §5.1 verbatim.
func FormatAsyncCompletion(completion AsyncCompletion) string {
	content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
	if content == "" {
		content = "(no output)"
	}
	if completion.TimedOut {
		return fmt.Sprintf("Async job %s (%s) timed out after %ds and was stopped. Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, completion.TimeoutSeconds, content)
	}
	if completion.Stopped {
		return fmt.Sprintf("Async job %s (%s) was stopped (job_kill). Partial output before the stop:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Cancelled {
		return fmt.Sprintf("Async job %s (%s) was cancelled (session stopped). Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Interrupted {
		return fmt.Sprintf("Async job %s (%s) was interrupted: its host process ended before the job finished.\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
}

// notifyAsyncCompletion is the workLedger.onWebDone callback: the job's
// terminal transition already committed its
// async_jobs row with delivery='pending' (work_ledger_transition.go) --
// there is no text/NoticeKind left to build here (doc sec.3.3: the driver's
// pull reconstructs both from the row at pull time, agent_notice_pull.go).
// This is now just the non-blocking wake HINT (doc sec.3.4): submit a Drain
// call to the session's owning driver iff the committed row wants a wake.
// See wakeSession's own doc for why a driver-owned child session's wake must
// never run on c.currentAgent (task #1049).
func (c *coordinator) notifyAsyncCompletion(completion AsyncCompletion) {
	// No ctx tagging needed anymore: a Drain call's own constructor
	// (newDrainCall) unconditionally stamps AutoResumed/BackgroundJobNotice/
	// NoticeKind itself, and a Drain never creates a user message (empty
	// Prompt), so there is no Origin left for a caller-supplied ctx to
	// influence either.
	// Supervision progress is recorded when the notice moves into history
	// (agent_notice_pull.go), not here.
	go func() {
		// Re-check trigger (ii)/(iv): this goroutine IS the delivery that
		// the completed job woke -- either directly (mailbox was idle) or
		// merged into an already-live/queued generation (mailbox was busy).
		// Re-evaluate any delegation parked for this session once wakeSession
		// returns, so a release can never land in the window before the
		// wake's effect is visible. wakeSession itself already logs+persists
		// a visible marker on failure; there is nothing else to do with its
		// error here.
		defer c.noteSubAgentChildRunEnded(completion.SessionID)
		if completion.Wake {
			_ = c.wakeSession(context.Background(), completion.SessionID, true)
		}
	}()
}

// Phase-4 step 4 (doc sec.3.5): ClaimAsyncCompletions/NextAsyncCompletion/
// HasPendingAsyncJobs and the in-memory ready queue they read are gone.
// `rush run`'s loop (internal/app/app_run_async.go) now claims/releases the
// external-driver marker directly via ClaimExternalDriver/ReleaseExternalDriver
// (coordinator_reaction_source.go) and re-derives its next turn from
// CLIScope against the DB, waiting on WaitForHint (with
// its bounded 5s fallback) instead of draining a memory queue.

// backgroundJobSummary formats a finished background command for injection
// into the owning session. Pure and deterministic so it can be unit-tested
// without a running shell.
func backgroundJobSummary(id, command string, stdout, stderr string, exitCode int, elapsed time.Duration) string {
	out := strings.TrimSpace(strings.Join(filterNonEmpty(stdout, stderr), "\n"))
	out = tools.TruncateOutput(out)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("Background job %s (`%s`) finished: exit %d, ran %s.\n\n%s",
		id, command, exitCode, elapsed.Round(time.Second), out)
}

// notifyBackgroundJobDone handles an SDK background shell's completion
// (doc sec.2: a notice with no async_jobs row). Step 3: the fact is
// persisted FIRST, durably, as a session_notices row (kind
// NoticeKindBGShellDone, wake=1 per the wake-policy table -- the row's own
// wake bit does not depend on session policy, only whether a TURN is forced
// right now does). AutoResumeOnJobDone decides ONLY that immediate-turn
// question for a session that is not a delegated child, exactly as today: on,
// a Drain call is submitted right away; off, the notice simply waits for the
// next natural turn/Drain to pull it (its wake=1 still counts then). A child
// whose delegation row runs is woken regardless (delegatedChildDriven).
func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())

	// The row and the slot decision are one step (persistBGShellCompletion).
	claimed := c.persistBGShellCompletion(sessionID, sh.ID, summary)
	// A delegated child is driven by its delegation, not by the auto-resume
	// policy (R6B-1): the claim (web only, AutoResumeOnJobDone on, a free slot)
	// bounds a session's OWN chain of automatic turns. Refusing a child's wake
	// for it left the child's debt owed with nothing to launch it while the
	// parent's delegation waited. drainPermitted stays the single launch gate
	// (a running row allows the turn, a released child is refused there); a root
	// keeps the cap and the policy.
	driven := false
	if !claimed {
		var err error
		if driven, err = c.delegatedChildDriven(context.Background(), sessionID); err != nil {
			slog.Warn("bg-shell completion: delegation state unreadable", "session_id", sessionID, "err", err)
			if !driven {
				c.addToRecheckSet(sessionID) // the 60s pass decides it
			}
		}
	}
	if claimed || driven {
		// Autonomous idle-resume: start (or, if busy, queue) a Drain call
		// over the just-persisted notice. The bound was spent by
		// claimAutoResume (reset by any human message); a driven child spends
		// no slot.
		slog.Info("Phase 4: resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID, "delegated_child", driven,
			"consecutive", c.consecutiveResume(sessionID))
		ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
		ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
		// The slot was spent ONCE, synchronously, at admission (before the
		// wake's goroutine spawns), so a burst of near-simultaneous
		// completions is bounded deterministically: at most
		// maxConsecutiveAutoResumes completions per human message launch a
		// turn of their own. The launch decision of this wake does not
		// re-check the counter (R2B-16); a paced or refused launch keeps its
		// slot and submits nothing.
		c.recheckWakes.Add(1) // waitRecheckWakes covers this detached wake too
		go func() {
			defer c.recheckWakes.Done()
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this goroutine is the delivery it woke.
			// Re-evaluate any delegation parked for this session once
			// wakeSession returns, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			defer c.noteSubAgentChildRunEnded(sessionID)
			_ = c.wakeSession(ctx, sessionID, true)
		}()
		return
	}

	// AutoResumeOnJobDone off (doc sec.3.4's session-policy table): the
	// notice is already durable and web-visible on the next pull; no turn
	// is forced here, matching today's behavior.
	c.noteSubAgentChildRunEnded(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

// delegatedChildDriven reports whether sessionID is a delegation's child that
// its delegation currently drives: a RUNNING async_jobs row names it as the
// child. The durable row decides (another process may own the delegation; this
// process's ledger and driver registry can be empty), so a released child, whose
// row is terminal, is not driven even while its driver is still registered.
// An unreadable row falls back to the driver registration, which only ever
// exists for a child, and reports the error: the launch decision that follows
// (wakeSession) is drainPermitted's, which refuses whatever the policy refuses.
func (c *coordinator) delegatedChildDriven(ctx context.Context, sessionID string) (bool, error) {
	if c.asyncJobs == nil {
		return false, nil
	}
	running, err := c.asyncJobs.hasRunningDelegationFor(ctx, sessionID)
	if err == nil {
		return running, nil
	}
	_, registered := c.subAgentDrivers.get(sessionID)
	return registered, err
}
