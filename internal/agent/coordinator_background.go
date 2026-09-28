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
	"github.com/PHPCraftdream/rush/internal/session"
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
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
}

// notifyAsyncCompletion is the AsyncCompletionSource callback wired into
// workLedger.onWebDone: the job's terminal transition already committed its
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
	id := jobIdentity{owner: completion.SessionID, toolCallID: completion.ToolCallID}
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
		_ = c.wakeSession(context.Background(), id, completion.Wake)
	}()
}

// ClaimAsyncCompletions makes sessionID's CLI completions queue for
// NextAsyncCompletion instead of waking the session.
func (c *coordinator) ClaimAsyncCompletions(sessionID string) {
	if c.asyncJobs != nil {
		c.asyncJobs.markDrained(sessionID)
	}
}

func (c *coordinator) NextAsyncCompletion(ctx context.Context, sessionID string) (AsyncCompletion, bool, error) {
	if c.asyncJobs == nil {
		return AsyncCompletion{}, false, nil
	}
	return c.asyncJobs.next(ctx, sessionID)
}

func (c *coordinator) HasPendingAsyncJobs(sessionID string) bool {
	return c.asyncJobs != nil && c.asyncJobs.pending(sessionID)
}

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
// question, exactly as today: on, a Drain call is submitted right away; off,
// the notice simply waits for the next natural turn/Drain to pull it (its
// wake=1 still counts then).
func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())

	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		insertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := c.asyncJobs.store.InsertSessionNotice(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
		cancel()
		if err != nil {
			slog.Error("failed to persist background-shell-done notice",
				"session_id", sessionID, "shell_id", sh.ID, "err", err)
		}
	}

	id := jobIdentity{owner: sessionID, toolCallID: sh.ID}
	if c.autoResumeEligible(sessionID) {
		// Autonomous idle-resume: start (or, if busy, queue) a Drain call
		// over the just-persisted notice. The bound is incremented per
		// completion (conservative: a coalesced queued completion still
		// counts toward the cap, which only makes runaway protection
		// stricter). Reset by any human message.
		c.bumpConsecutiveResume(sessionID)
		slog.Info("Phase 4: auto-resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID,
			"consecutive", c.consecutiveResume(sessionID))
		ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
		ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
		go func() {
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this goroutine is the delivery it woke.
			// Re-evaluate any delegation parked for this session once
			// wakeSession returns, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			defer c.noteSubAgentChildRunEnded(sessionID)
			_ = c.wakeSession(ctx, id, true)
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
