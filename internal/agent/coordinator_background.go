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
	"github.com/PHPCraftdream/rush/internal/message"
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
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
}

// notifyAsyncCompletion is the AsyncCompletionSource callback wired into
// workLedger.onWebDone: it builds the notice text/kind and delegates the
// entire delivery (persist-then-wake, driver selection) to wakeSession --
// see wakeSession's own doc for why a driver-owned child session's wake must
// never run on c.currentAgent (task #1049).
func (c *coordinator) notifyAsyncCompletion(completion AsyncCompletion) {
	ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
	ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
	origin := message.OriginWeb
	if completion.cli {
		origin = message.OriginCLI
	}
	ctx = WithCallOrigin(ctx, origin)

	noticeKind := ""
	if completion.TimedOut {
		noticeKind = "timeout_terminated"
	}
	id := jobIdentity{owner: completion.SessionID, toolCallID: completion.ToolCallID}
	text := FormatAsyncCompletion(completion)
	go func() {
		// Re-check trigger (ii)/(iv): this goroutine IS the delivery that
		// the completed job woke -- either directly (mailbox was idle) or
		// merged into an already-live/queued generation (mailbox was busy).
		// Re-evaluate any delegation parked for this session once wakeSession
		// returns, so a release can never land in the window before the
		// wake's effect is visible. wakeSession itself already logs+persists
		// a visible marker on failure (§1.4); there is nothing else to do
		// with its error here.
		defer c.noteSubAgentChildRunEnded(completion.SessionID)
		_ = c.wakeSession(ctx, id, text, noticeKind, true)
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

// notifyBackgroundJobDone is invoked from a BackgroundShell.OnDone goroutine
// once a backgrounded bash command reaches a terminal state. It builds a
// concise summary and either (Phase 4, when autonomy is eligible) starts a
// fresh turn over it, or (Phase 3 fallback) pushes it into the owning session
// via InjectMessage. Detached: the OnDone goroutine outlives the turn that
// started it, so we never block or cancel the agent. Delivery failures (e.g.
// session closed) are logged at debug level.
func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shell.BackgroundShell) {
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())

	if c.autoResumeEligible(sessionID) {
		// Autonomous idle-resume: start (or, if busy, queue — single-flight via
		// sessionAgent.Run) a fresh turn over the completion summary. The bound
		// is incremented per completion (conservative: a coalesced queued
		// completion still counts toward the cap, which only makes runaway
		// protection stricter). Reset by any human message.
		c.bumpConsecutiveResume(sessionID)
		slog.Info("Phase 4: auto-resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID,
			"consecutive", c.consecutiveResume(sessionID))
		// Detached + cancelable: outlives the OnDone goroutine; the turn's
		// own watchdog/Cancel(sessionID) governs its lifetime, so NO short
		// timeout here (unlike the InjectMessage path — a turn can be long).
		// Tag the context so the persisted user message is marked
		// AutoResumed and rendered with a badge in the web UI. Also tag it
		// as a BackgroundJobNotice so the web shows the notice badge (an
		// auto-resume is also a job-completion notice).
		ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
		ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
		id := jobIdentity{owner: sessionID, toolCallID: sh.ID}
		go func() {
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this goroutine is the delivery it woke.
			// Re-evaluate any delegation parked for this session once
			// wakeSession returns, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			// wakeSession's own defer recover() covers a panic in the run it
			// starts; nothing else to do with its error here (already
			// logged+persisted, §1.4).
			defer c.noteSubAgentChildRunEnded(sessionID)
			_ = c.wakeSession(ctx, id, summary, "", true)
		}()
		return
	}

	// Phase 3 behavior (unchanged): persist + (if busy) merge into the running
	// turn; if idle, just persisted + web-visible, no auto-turn. Tag the context
	// so the injected user message is flagged as a BackgroundJobNotice.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
	defer cancel()
	id := jobIdentity{owner: sessionID, toolCallID: sh.ID}
	if err := c.wakeSession(ctx, id, summary, "", false); err != nil {
		slog.Warn("background job completion not delivered (session likely closed)",
			"session_id", sessionID,
			"shell_id", sh.ID,
			"err", err)
	}
	// Re-check trigger (iii), Phase 3 branch: same reasoning as above.
	c.noteSubAgentChildRunEnded(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}
