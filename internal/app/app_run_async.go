package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// The `rush run` loop follows the coordinator's launch decision (docs/reviews/
// 2026-09-30-async-phase4-round2-attempts-design.md sec.1.6): CLIScope answers
// what to do next -- run a Drain (Owed), wait for a paced retry, exit on
// deferred/stuck debt -- and the loop never accounts an attempt itself: the
// turn loop that ran the Drain does (agent/drain_attempt.go).
var (
	// cliDBRetryPause bounds how fast the loop re-polls after a transient
	// scope-read failure (doc sec.3.5: "a DB read error is a retry with a
	// pause, never a silent skip").
	cliDBRetryPause = 500 * time.Millisecond
	// cliLockBusyRetryOverallLimit bounds how long a run of consecutive Drain
	// REFUSALS (session lock held by another process, an unwritable lock dir,
	// a provider that is not configured, peak hours, ...) or setup failures is
	// retried before the run gives up with that error. A refusal is paced by
	// the launch gate (drainRefusalPauseLoop), a setup failure by
	// cliSetupRetryPause; neither by anything else in this loop.
	cliLockBusyRetryOverallLimit = 30 * time.Second
	// cliQueuedDrainPause is the wait after a Drain that queued behind another
	// owner (it ran no turn of its own and cannot be accounted here).
	cliQueuedDrainPause = 500 * time.Millisecond
	// cliSetupRetryPause paces the retry of a Drain that failed in setup before
	// any turn (no launch gate stands behind such a failure; the streak is
	// bounded by cliLockBusyRetryOverallLimit).
	cliSetupRetryPause = 500 * time.Millisecond
)

// cliLoopTurnDoneSeam is a test-only hook called right after each loop turn's
// ExecuteRun returns, before its result is classified. Lets a test land a
// cancellation exactly between a finished turn and the loop's next decision.
// nil in every production path.
var cliLoopTurnDoneSeam func()

// flushLoopExit renders final's envelope through output exactly the way the
// loop's normal "scope closed" exit always did: every exit from the loop --
// refusal give-up, ctx cancellation, a wait error, a stuck debt, or the
// ordinary scope-closed end -- flushes a REAL prior turn's terse text or JSON
// envelope through the SAME path. final == nil is a no-op.
func flushLoopExit(output io.Writer, mode RunMode, final *RunResult, lastBuffered *bytes.Buffer) error {
	if final == nil {
		return nil
	}
	switch mode {
	case RunModeTerse:
		if _, err := io.Copy(output, lastBuffered); err != nil {
			return err
		}
	case RunModeJSON:
		if err := json.NewEncoder(output).Encode(final); err != nil {
			return fmt.Errorf("failed to encode JSON result: %w", err)
		}
	}
	return nil
}

// sleepOrCtxDone sleeps for d, or returns false early if ctx is done first.
func sleepOrCtxDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// loopTotals accumulates what EVERY real turn of one invocation did, so any
// exit path reports the whole run and not only its last turn: usage, tool
// calls, warnings and sub-agent outputs (a cancelled or failed Drain adds its
// usage without replacing the last completed answer).
type loopTotals struct {
	tokens          int64
	cost            float64
	counts          map[string]int
	warnings        []string
	subAgentOutputs []SubAgentOutput
}

func (t *loopTotals) add(r *RunResult) {
	if r == nil {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	t.tokens += r.Usage.DeltaTokens
	t.cost += r.Usage.DeltaCostUSD
	t.warnings = append(t.warnings, r.Warnings...)
	t.subAgentOutputs = append(t.subAgentOutputs, r.SubAgentOutputs...)
	for _, stat := range r.ToolCalls {
		t.counts[stat.Name] += stat.Count
	}
}

func (t *loopTotals) applyTo(final *RunResult, started time.Time) {
	final.Usage.DeltaTokens = t.tokens
	final.Usage.DeltaCostUSD = t.cost
	final.Warnings = t.warnings
	final.SubAgentOutputs = t.subAgentOutputs
	final.DurationMs = time.Since(started).Milliseconds()
	final.ToolCalls = final.ToolCalls[:0]
	for name, count := range t.counts {
		final.ToolCalls = append(final.ToolCalls, ToolCallStat{Name: name, Count: count})
	}
	slices.SortFunc(final.ToolCalls, func(a, b ToolCallStat) int { return cmpName(a.Name, b.Name) })
}

// cliLoop is one `rush run` invocation that follows the coordinator's launch
// decisions: a first turn (the user's request), then Drain turns while the
// session owes a reaction and work is still running.
type cliLoop struct {
	app    *App
	source agent.ReactionDebtSource
	ctx    context.Context
	output io.Writer
	mode   RunMode

	hideSpinner       bool
	overrides         RunOverrides // the caller's, incl. OnFinishHook (run by the caller)
	turnOverrides     RunOverrides // per-turn: no OnFinishHook
	prompt            string
	continueSessionID string
	useLast           bool
	started           time.Time

	sessionID        string
	driverClaimed    bool
	claimedSessionID string

	final        *RunResult    // the last COMPLETED turn's result (carries the answer)
	runErr       error         // the last real turn's outcome
	lastFailed   *RunResult    // the last real Drain's result, when it failed
	lastBuffered *bytes.Buffer // terse output of the last completed turn
	tot          loopTotals

	refusalSince time.Time
	// lastOpenScopeNotice paces the wait heartbeat across the WHOLE run, not
	// per nextStep call (which restarts after every Drain).
	lastOpenScopeNotice time.Time
	stderr              io.Writer
}

// cliLoopStderr, when non-nil, replaces os.Stderr (read at each write, like
// the direct uses it replaced) as the loop's diagnostics sink; a test seam.
var cliLoopStderr io.Writer

func (l *cliLoop) errOut() io.Writer {
	if l.stderr != nil {
		return l.stderr
	}
	if cliLoopStderr != nil {
		return cliLoopStderr
	}
	return os.Stderr
}

func (app *App) runNonInteractiveWithAsyncResults(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (final *RunResult, runErr error) {
	if output == nil {
		output = io.Discard
	}
	source, isReactionSource := app.AgentCoordinator.(agent.ReactionDebtSource)
	if !isReactionSource || overrides.Origin != message.OriginCLI {
		final, runErr = app.ExecuteRun(ctx, RunRequest{
			Prompt: prompt, Overrides: overrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: output, Stderr: os.Stderr,
			HideSpinner: hideSpinner,
		})
		if mode == RunModeJSON && final != nil {
			if err := json.NewEncoder(output).Encode(final); err != nil {
				return final, fmt.Errorf("failed to encode JSON result: %w", err)
			}
		}
		return final, runErr
	}

	// B12/C14 fix, part 3: run the dead-host sweep and retention purge once,
	// best-effort, per invocation (the 60s pass started by the driver claim
	// repeats them while the loop waits).
	source.RunMaintenanceSweep(ctx)

	started := time.Now()
	turnOverrides := overrides
	turnOverrides.OnFinishHook = ""
	defer func() {
		if overrides.OnFinishHook != "" && final != nil {
			runOnFinishHook(overrides.OnFinishHook, final.SessionID, final.ExitReason,
				final.Usage.DeltaCostUSD, final.Usage.DeltaTokens, time.Since(started))
		}
	}()
	l := &cliLoop{
		app: app, source: source, ctx: ctx, output: output, mode: mode,
		hideSpinner: hideSpinner, overrides: overrides, turnOverrides: turnOverrides,
		prompt: prompt, continueSessionID: continueSessionID, useLast: useLast,
		started: started, sessionID: continueSessionID, lastBuffered: &bytes.Buffer{},
	}
	// Doc sec.3.4: this loop IS the external driver for its session -- claimed
	// (durably, so no other process starts a reaction turn for it) as soon as
	// the session id resolves, released on every exit on a context detached
	// from ctx: Ctrl-C must still delete the marker.
	defer func() {
		if l.driverClaimed {
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer relCancel()
			source.ReleaseExternalDriver(relCtx, l.claimedSessionID)
		}
	}()
	return l.run()
}

// claim is ExecuteRun's onSessionResolved: it claims the driver marker once.
func (l *cliLoop) claim(resolved string) error {
	if !l.driverClaimed {
		if err := l.source.ClaimExternalDriver(l.ctx, resolved); err != nil {
			return err
		}
		l.driverClaimed = true
		l.claimedSessionID = resolved
	}
	l.sessionID = resolved
	return nil
}

// runTurn runs one ExecuteRun: the first call carries the user's request, every
// later one is a Drain (empty prompt, mutation-free setup).
func (l *cliLoop) runTurn(first bool) (*RunResult, *bytes.Buffer, error) {
	buffered := &bytes.Buffer{}
	turnOutput := l.output
	switch l.mode {
	case RunModeTerse:
		turnOutput = buffered
	case RunModeJSON:
		turnOutput = io.Discard
	}
	turnCtx := l.ctx
	if !first {
		// Doc sec.3.4: an empty-prompt Drain call: lifts ErrEmptyPrompt, skips
		// createUserMessage, reacts only to what its own turn-start pull moves
		// into history.
		turnCtx = agent.WithDrainCall(agent.WithBackgroundJobNotice(l.ctx))
	}
	result, err := l.app.ExecuteRun(turnCtx, RunRequest{
		Prompt: l.prompt, Overrides: l.turnOverrides, Mode: l.mode,
		ContinueSessionID: l.continueSessionID, UseLast: l.useLast,
		Origin: l.overrides.Origin, Stdout: turnOutput, Stderr: l.errOut(),
		HideSpinner:       l.hideSpinner,
		captureResult:     true,
		drainTurn:         !first,
		onSessionResolved: l.claim,
	})
	if cliLoopTurnDoneSeam != nil {
		cliLoopTurnDoneSeam()
	}
	return result, buffered, err
}

func (l *cliLoop) run() (*RunResult, error) {
	result, buffered, err := l.runTurn(true)
	// The session never resolved or its driver claim was refused (another live
	// loop drives it): the first turn never ran, so there is no scope of ours to
	// wait on -- fail now, having changed nothing.
	if err != nil && !l.driverClaimed {
		return nil, err
	}
	l.final, l.runErr, l.lastBuffered = result, err, buffered
	l.tot.add(result)
	if result != nil {
		l.sessionID = result.SessionID
	}
	// Every later turn is a Drain: empty prompt, on the resolved session.
	l.prompt, l.continueSessionID, l.useLast = "", l.sessionID, false
	// Doc sec.3.8: a "session lock busy" refusal of the user's own turn fails
	// fast (pre-phase-4 contract, sessionBusyGuidance): the envelope is still
	// flushed and --on-finish still runs (R2C-2).
	var lockBusy *session.SessionLockBusyError
	if errors.As(err, &lockBusy) {
		return l.exit(err, "")
	}
	if l.sessionID == "" || l.ctx.Err() != nil {
		return l.exitCanceled()
	}

	for {
		step, why, waitErr := l.nextStep()
		switch {
		case waitErr != nil:
			return l.exitWait(waitErr)
		case step == stepExit:
			return l.exit(l.runErr, "")
		case step == stepStuck:
			fmt.Fprintf(l.errOut(), "rush run: session %q has a notice the loop stopped reacting to after repeated failed attempts (%s); it stays pending -- see `rush sessions why %s`\n", l.sessionID, why, l.sessionID)
			return l.exit(&runIncompleteError{reason: "error", detail: "a notice could not be reacted to: " + why}, "error")
		}
		if err := l.precheck(); err != nil {
			return l.exitPrecheck(err)
		}

		usageBefore := l.sessionUsage()
		result, buffered, err = l.runTurn(false)
		if l.ctx.Err() != nil {
			l.tot.add(result)
			if result == nil {
				// An interrupted Drain returns no envelope; its usage is what the
				// session totals gained meanwhile.
				l.tot.addSince(usageBefore, l.sessionUsage())
			}
			return l.exitCanceled()
		}
		// A Drain that crossed the run's budget ends the run: its result is not the
		// answer, and no further paid turn follows.
		if !agent.IsDrainNotAttempted(err) && !errors.Is(err, ErrRunQueued) {
			if capErr := l.capError(); capErr != nil {
				l.tot.add(result)
				return l.exitPrecheck(capErr)
			}
		}
		if done, exitErr := l.afterDrain(result, err, buffered); done {
			return l.exit(exitErr, "error")
		}
	}
}

// afterDrain classifies one finished Drain iteration. done reports that the
// loop must end now (the refusal budget ran out) with exitErr.
func (l *cliLoop) afterDrain(result *RunResult, err error, buffered *bytes.Buffer) (done bool, exitErr error) {
	var awaiting *agent.AwaitingAnswerError
	switch {
	case errors.Is(err, ErrRunQueued):
		// It queued behind another owner (or the Drain found nothing visible to
		// react to): no turn of ours to account -- the owner's loop already did.
		l.refusalSince = time.Time{}
		l.source.WaitForHint(l.ctx, l.sessionID, time.Now().Add(cliQueuedDrainPause))
		return false, nil
	case agent.IsDrainNotAttempted(err), result == nil && err != nil && !errors.As(err, &awaiting):
		// A refusal is never counted or settled: the launch gate paces the retry
		// (0.5s for this loop's own session); the loop only bounds the streak.
		// A setup failure before any turn (no result: a model override that no
		// longer resolves, a run-queue read error, ...) has no gate behind it,
		// so it takes the same bounded/visible path and the loop paces it
		// itself; unbounded it would relaunch as fast as it fails.
		refused := agent.IsDrainNotAttempted(err)
		first, still := "was refused", "is still refused"
		if !refused {
			first, still = "could not be set up", "still cannot be set up"
		}
		if l.refusalSince.IsZero() {
			l.refusalSince = time.Now()
			fmt.Fprintf(l.errOut(), "rush run: session %q: the reaction turn %s (%v); retrying\n", l.sessionID, first, err)
		}
		if time.Since(l.refusalSince) > cliLockBusyRetryOverallLimit {
			fmt.Fprintf(l.errOut(), "rush run: session %q: the reaction turn %s after %s (%v); giving up\n",
				l.sessionID, still, cliLockBusyRetryOverallLimit, err)
			return true, err
		}
		if !refused {
			sleepOrCtxDone(l.ctx, cliSetupRetryPause)
		}
		return false, nil
	}
	l.refusalSince = time.Time{}
	l.tot.add(result)
	switch {
	case err == nil || errors.As(err, &awaiting):
		// A completed turn (or a question -- an answer of its own kind): it is
		// now the run's answer.
		if result != nil {
			l.final = result
			l.lastBuffered = buffered
		}
		l.runErr, l.lastFailed = err, nil
	default:
		// A failed Drain never replaces the last completed answer; the error
		// exit carries its classification.
		l.runErr, l.lastFailed = err, result
	}
	return false, nil
}

// cliStep is what nextStep decided.
type cliStep int

const (
	stepExit cliStep = iota
	stepDrain
	stepStuck
)

// cliOpenScopeWaitNoticeInterval bounds how often the loop prints its "still
// waiting on open work" stderr heartbeat (C17 fix): the first line comes when
// the loop starts waiting, then at most one per interval over the WHOLE run
// (the cadence is not restarted by a Drain). An `Unknown` host-liveness
// verdict keeps a session's scope reported open for up to `sessions gc`'s 6h
// horizon with NO observable signal otherwise. A var so a test can shrink it.
var cliOpenScopeWaitNoticeInterval = 60 * time.Second

// cliDBErrorRetryOverallLimit bounds the DB-read-error retry loop (C17 fix): a
// persistent DB failure (disk issue, corruption) was previously retried
// forever with only a slog.Warn line. A var so a test can shrink it.
var cliDBErrorRetryOverallLimit = 30 * time.Second

// nextStep implements doc sec.3.5's CLI-loop decision through the
// coordinator's single CLIScope answer, waiting as long as it must: Owed --
// run a Drain (the pre-launch caps are checked by the caller); Paced -- wait
// until the gate reopens (a newer event may reopen it sooner); running work --
// wait (heartbeat on stderr); Stuck with nothing running -- give up; Deferred
// or no debt with nothing running -- the scope is closed. A DB read error
// retries with a pause, bounded overall and visible on stderr.
func (l *cliLoop) nextStep() (step cliStep, why string, err error) {
	var dbErrorRetryStart time.Time
	for {
		if l.ctx.Err() != nil {
			return stepExit, "", l.ctx.Err()
		}
		state, stateErr := l.source.CLIScope(l.ctx, l.sessionID)
		if stateErr != nil {
			slog.Warn("rush run: scope check failed; retrying", "session_id", l.sessionID, "err", stateErr)
			if dbErrorRetryStart.IsZero() {
				dbErrorRetryStart = time.Now()
			}
			if time.Since(dbErrorRetryStart) > cliDBErrorRetryOverallLimit {
				fmt.Fprintf(l.errOut(), "rush run: session %q's database has been unreadable for %s (%s); giving up\n",
					l.sessionID, cliDBErrorRetryOverallLimit, stateErr)
				return stepExit, "", stateErr
			}
			if !sleepOrCtxDone(l.ctx, cliDBRetryPause) {
				return stepExit, "", l.ctx.Err()
			}
			continue
		}
		dbErrorRetryStart = time.Time{}
		switch {
		case state.Drain == agent.DrainOwed:
			return stepDrain, "", nil
		case state.Drain == agent.DrainPaced:
			l.source.WaitForHint(l.ctx, l.sessionID, state.RetryAt)
			continue
		case state.WorkOpen:
			if now := time.Now(); l.lastOpenScopeNotice.IsZero() || now.Sub(l.lastOpenScopeNotice) >= cliOpenScopeWaitNoticeInterval {
				l.lastOpenScopeNotice = now
				fmt.Fprintf(l.errOut(), "rush run: session %q still has open work; waiting on %s\n", l.sessionID, l.describeOpenWork())
			}
			l.source.WaitForHint(l.ctx, l.sessionID, time.Time{})
			continue
		case state.Drain == agent.DrainStuck:
			return stepStuck, state.Reason, nil
		case state.Drain == agent.DrainDeferred:
			fmt.Fprintf(l.errOut(), "rush run: session %q has a notice no automatic turn is allowed for (%s); it stays for the next turn\n", l.sessionID, state.Reason)
		}
		return stepExit, "", nil
	}
}

// precheck refuses to launch a paid reaction turn once the run's own budget or
// an operator cancel already ended it: --max-cost/--max-tokens compare exactly
// like enforceRunawayCaps, and `sessions cancel` between turns ends the run.
func (l *cliLoop) precheck() error {
	if err := l.capError(); err != nil {
		return err
	}
	if canceled, err := l.app.Sessions.IsCancelRequested(l.ctx, l.sessionID); err == nil && canceled {
		return &runIncompleteError{reason: "canceled", detail: fmt.Sprintf("session %s cancelled by user", l.sessionID)}
	}
	return nil
}

// exit ends the loop: totals of every real turn are applied to the final
// envelope, the envelope is flushed through the one common path, and the
// caller gets (final, err). reason, when set, is the exit_reason to record for
// err; otherwise a failed last Drain's own classification is carried over.
func (l *cliLoop) exit(err error, reason string) (*RunResult, error) {
	final := l.final
	if final != nil {
		l.tot.applyTo(final, l.started)
		switch {
		case err != nil && reason != "":
			final.ExitReason = reason
			final.Error = err.Error()
		case err != nil && l.lastFailed != nil && errors.Is(err, l.runErr):
			final.ExitReason = l.lastFailed.ExitReason
			final.Error = l.lastFailed.Error
		}
	}
	if flushErr := flushLoopExit(l.output, l.mode, final, l.lastBuffered); flushErr != nil {
		return final, flushErr
	}
	return final, err
}

func (l *cliLoop) exitCanceled() (*RunResult, error) {
	err := l.runErr
	if err == nil {
		err = l.ctx.Err()
	}
	if l.final != nil && l.ctx.Err() != nil {
		l.final.ExitReason = "canceled"
		l.final.Error = l.ctx.Err().Error()
	}
	return l.flushed(err)
}

// flushed applies the totals and flushes without touching the exit reason.
func (l *cliLoop) flushed(err error) (*RunResult, error) {
	if l.final != nil {
		l.tot.applyTo(l.final, l.started)
	}
	if flushErr := flushLoopExit(l.output, l.mode, l.final, l.lastBuffered); flushErr != nil {
		return l.final, flushErr
	}
	return l.final, err
}

func (l *cliLoop) exitWait(waitErr error) (*RunResult, error) {
	if l.final != nil {
		l.final.ExitReason = "canceled"
		if !errors.Is(waitErr, context.Canceled) && !errors.Is(waitErr, context.DeadlineExceeded) {
			// A persistent DB-read failure is not a cancellation.
			l.final.ExitReason = "error"
		}
		l.final.Error = waitErr.Error()
	}
	return l.flushed(waitErr)
}

func (l *cliLoop) exitPrecheck(err error) (*RunResult, error) {
	var inc *runIncompleteError
	reason := "error"
	if errors.As(err, &inc) {
		reason = inc.reason
	}
	return l.exit(err, reason)
}

func cmpName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// capError reports the run's --max-cost/--max-tokens as exceeded, comparing
// the session totals exactly like the agent's enforceRunawayCaps. A single-step
// turn that crosses a cap is not cut short (fantasy ignores the step
// callback's error), so the loop checks after every Drain too.
func (l *cliLoop) capError() error {
	if l.overrides.MaxCost <= 0 && l.overrides.MaxTokens <= 0 {
		return nil
	}
	sess, err := l.app.Sessions.Get(l.ctx, l.sessionID)
	if err != nil {
		return nil
	}
	if l.overrides.MaxCost > 0 && sess.Cost > l.overrides.MaxCost {
		return &runIncompleteError{reason: "error", detail: fmt.Sprintf(
			"session %s aborted: cost $%.4f exceeds max $%.4f", l.sessionID, sess.Cost, l.overrides.MaxCost)}
	}
	if total := sess.PromptTokens + sess.CompletionTokens; l.overrides.MaxTokens > 0 && total > l.overrides.MaxTokens {
		return &runIncompleteError{reason: "error", detail: fmt.Sprintf(
			"session %s aborted: %d tokens exceeds max %d", l.sessionID, total, l.overrides.MaxTokens)}
	}
	return nil
}

// usageMark is the session's token and cost totals at one instant.
type usageMark struct {
	tokens int64
	cost   float64
	ok     bool
}

// sessionUsage reads the session totals on a context that survives Ctrl-C.
func (l *cliLoop) sessionUsage() usageMark {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	sess, err := l.app.Sessions.Get(ctx, l.sessionID)
	if err != nil {
		return usageMark{}
	}
	return usageMark{tokens: sess.PromptTokens + sess.CompletionTokens, cost: sess.Cost, ok: true}
}

// addSince adds what the session totals gained between two marks.
func (t *loopTotals) addSince(before, after usageMark) {
	if !before.ok || !after.ok {
		return
	}
	t.tokens += max(after.tokens-before.tokens, 0)
	t.cost += max(after.cost-before.cost, 0)
}
