// Background-job completion notices, Phase 4 autonomous idle-resume, and
// busy-state queries. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"charm.land/fantasy"
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
func FormatAsyncCompletion(completion AsyncCompletion) string {
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
	if content == "" {
		content = "(no output)"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
}

func (c *coordinator) notifyAsyncCompletion(completion AsyncCompletion) {
	ctx := context.WithValue(context.Background(), autoResumedCtxKey{}, true)
	ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
	origin := message.OriginWeb
	if completion.cli {
		origin = message.OriginCLI
	}
	ctx = WithCallOrigin(ctx, origin)

	// One driver per session (task #1049; design doc
	// docs/plans/2026-09-27-async-structured-concurrency.md §3): a delegated
	// child session's own async/background completion must wake the SAME
	// SessionAgent that is driving its turns, never c.currentAgent's coder
	// agent -- a different SessionAgent has no mailbox record of this child
	// session, treats it as idle, and races the driver's own in-flight Run
	// for the same session on the shared OS-level session lock instead of
	// queuing behind it. c.Run (-> c.currentAgent.Run) remains correct for
	// every session that IS driven by the coder agent (the root, and any
	// non-delegated session), since no driver is ever registered for those.
	notify := func(ctx context.Context) (*fantasy.AgentResult, error) {
		return c.Run(ctx, completion.SessionID, FormatAsyncCompletion(completion))
	}
	if driver, ok := c.subAgentDrivers.get(completion.SessionID); ok {
		call := driver.callFor(FormatAsyncCompletion(completion))
		// c.Run's buildCall stamps these three fields from ctx
		// (noticeFlagsFrom/CallOriginFrom); driver.agent.Run bypasses
		// buildCall entirely, so they must be set explicitly here to keep the
		// persisted notice's badges/origin identical to the c.Run path.
		call.AutoResumed = true
		call.BackgroundJobNotice = true
		call.Origin = origin
		notify = func(ctx context.Context) (*fantasy.AgentResult, error) {
			return driver.agent.Run(ctx, call)
		}
	}
	go runAutoResumeRecovered(ctx, completion.SessionID, completion.ToolCallID, func(ctx context.Context) (*fantasy.AgentResult, error) {
		// Re-check trigger (ii)/(iv): this closure IS the turn that the
		// completed job woke -- either directly (mailbox was idle) or as the
		// eventual return of a Run() call that only queued the wake behind an
		// already-live turn on the SAME driver (mailbox was busy; the queued
		// call is drained by that live turn's own end-of-turn loop, see
		// agent_run.go's runOwned). Either way, re-evaluate any delegation
		// parked for this session only once this call returns, so a release
		// can never land in the window before the wake's effect (queued or
		// direct) is visible.
		defer c.noteSubAgentChildRunEnded(completion.SessionID)
		return notify(ctx)
	})
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
		go runAutoResumeRecovered(ctx, sessionID, sh.ID, func(ctx context.Context) (*fantasy.AgentResult, error) {
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this closure is the turn it woke.
			// Re-evaluate any delegation parked for this session once this
			// run has returned, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			defer c.noteSubAgentChildRunEnded(sessionID)
			return c.Run(ctx, sessionID, summary)
		})
		return
	}

	// Phase 3 behavior (unchanged): persist + (if busy) merge into the running
	// turn; if idle, just persisted + web-visible, no auto-turn. Tag the context
	// so the injected user message is flagged as a BackgroundJobNotice.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	ctx = context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
	defer cancel()
	if _, err := c.InjectMessage(ctx, sessionID, summary); err != nil {
		slog.Debug("background job completion not delivered (session likely closed)",
			"session_id", sessionID,
			"shell_id", sh.ID,
			"err", err)
	}
	// Re-check trigger (iii), Phase 3 branch: same reasoning as above.
	c.noteSubAgentChildRunEnded(sessionID)
}

// runAutoResumeRecovered runs runFn (normally a closure over c.Run for the
// Phase 4 auto-resume turn) with panic isolation, on the calling goroutine.
// Callers spawn this in its own goroutine (see notifyBackgroundJobDone)
// because it is independent of the BackgroundShell.OnDone goroutine that
// triggers it — OnDone's own recover() does not cover a panic raised in
// here, since by the time this runs it is a sibling goroutine, not a child
// call of OnDone's callback.
//
// runFn re-enters the full synchronous tool-dispatch chain (same call shape
// as app.go's RunNonInteractive goroutine, see runAgentTurnRecovered there),
// so any tool call made during this auto-resumed turn could panic exactly
// like it could during a human-initiated turn. Without this recover, such a
// panic would crash the whole rush process with no log output, at an
// arbitrary time long after the triggering background job completed.
func runAutoResumeRecovered(ctx context.Context, sessionID, shellID string, runFn func(ctx context.Context) (*fantasy.AgentResult, error)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Phase 4 auto-resume run panic",
				"session_id", sessionID, "shell_id", shellID,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	if _, err := runFn(ctx); err != nil {
		slog.Debug("Phase 4 auto-resume run failed (session likely closed)",
			"session_id", sessionID, "shell_id", shellID, "err", err)
	}
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}
