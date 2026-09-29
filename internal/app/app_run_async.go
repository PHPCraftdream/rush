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

// cliDBRetryPause/cliLockBusyRetryPause bound how fast the `rush run` loop
// re-polls after a transient failure (doc sec.3.5): "a DB read error is a
// retry with a pause, never a silent skip", and "a session-lock-busy
// refusal on its own session retries after a short bounded pause instead of
// exiting". Both are short enough that a real `rush run --timeout` still
// has room to fire, long enough that a genuinely stuck DB/lock doesn't spin
// the CPU.
// cliLockBusyRetryPause/cliLockBusyRetryOverallLimit are vars (not consts) so
// a test can shrink them and exercise the overall-limit branch at test
// timescale instead of a real 30s wait.
var (
	cliDBRetryPause       = 500 * time.Millisecond
	cliLockBusyRetryPause = 500 * time.Millisecond
	// cliLockBusyRetryOverallLimit bounds a Drain-context retry's total
	// budget for waiting out ANOTHER PROCESS's hold on this session's OS
	// lock (C1 fix): unlike the first (real user) turn, which now fails
	// fast on a busy lock, a Drain retry (this loop's own reaction-debt
	// turn) may legitimately race a short-lived foreign holder (e.g. a web
	// tab pulling notices) and is worth a bounded wait -- but never an
	// unbounded one.
	cliLockBusyRetryOverallLimit = 30 * time.Second
)

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

	// B12/C14 fix, part 3: a non-persistent coordinator (this one) never
	// starts the recurring 60s ticker, so a CLI-only install would otherwise
	// never sweep dead hosts or purge expired rows at all -- contradicting
	// the CHANGELOG/`--jobs-older-than` help's documented promise. Run the
	// sweep/purge halves once, best-effort, per invocation instead of a
	// recurring background ticker (which would keep an otherwise-short-lived
	// `rush run` process alive doing nothing between ticks).
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
	counts := make(map[string]int)
	var warnings []string
	var subAgentOutputs []SubAgentOutput
	var totalTokens int64
	var totalCost float64
	firstTurn := true
	// lockBusyRetryStart marks the beginning of the CURRENT run of
	// consecutive session-lock-busy retries (C1 fix) -- reset to zero the
	// instant a turn actually runs, so an overall bound applies to one
	// contiguous busy streak, not the whole (possibly long-lived) rush run
	// invocation.
	var lockBusyRetryStart time.Time
	// lastBuffered is the terse output of the last REAL turn (a no-turn Drain
	// iteration prints nothing and must not blank it).
	lastBuffered := &bytes.Buffer{}
	sessionID := continueSessionID
	// Doc sec.3.4: this loop IS the external driver for sessionID -- claimed
	// as soon as the session id resolves (onSessionResolved below), released
	// unconditionally on every exit so a later web-driven wake for the same
	// session id goes back to ordinary Drain-turn routing.
	defer func() {
		if sessionID != "" {
			source.ReleaseExternalDriver(sessionID)
		}
	}()
	for {
		buffered := &bytes.Buffer{}
		turnOutput := output
		switch mode {
		case RunModeTerse:
			turnOutput = buffered
		case RunModeJSON:
			turnOutput = io.Discard
		}
		turnCtx := ctx
		var drainSnapshot session.DebtSnapshot
		if !firstTurn {
			// Phase-4 (doc sec.3.4): this turn carries an empty prompt
			// (below) and must be built as a Drain call -- lifts
			// ErrEmptyPrompt, skips createUserMessage, and reacts only to
			// whatever this turn's own turn-start pull moves into history.
			turnCtx = agent.WithDrainCall(agent.WithBackgroundJobNotice(ctx))
			// Doc sec.6: the root's own turns never go through wakeSession,
			// so this loop captures/records settle-by-failure accounting
			// itself -- see RecordDrainTurnOutcome below.
			drainSnapshot = source.CaptureDrainSnapshot(ctx, sessionID)
		}
		result, err := app.ExecuteRun(turnCtx, RunRequest{
			Prompt: prompt, Overrides: turnOverrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: turnOutput, Stderr: os.Stderr,
			HideSpinner:   hideSpinner,
			captureResult: true,
			onSessionResolved: func(resolved string) {
				sessionID = resolved
				source.ClaimExternalDriver(resolved)
			},
		})

		// Doc sec.3.8: a "session lock busy" refusal on OUR OWN session
		// (another live process -- typically a web tab pulling notices --
		// holds the OS-level lock right now) retries this exact turn after
		// a short bounded pause instead of ending the run. Checked before
		// the ErrRunQueued/drainNoTurn classification below: this is a
		// DIFFERENT, cross-process refusal, never the normal in-process
		// "Drain queued behind an active call" signal.
		//
		// C1 fix: ExecuteRun is a per-invocation primitive that mutates the
		// session on every call (UpdateSystemPrompt, UpdateReasoningEffort,
		// ClearCancelRequest, SetBudget, SetEndedReason, ...) -- retrying it
		// silently and without bound against a session another process
		// currently owns repeats every one of those writes each pass,
		// including erasing a `sessions cancel` request landed in the same
		// window. The FIRST turn (the user's actual request; `rush run
		// --session <busy>`) fails fast instead, matching pre-phase-4
		// behavior and sessionBusyGuidance's own documented contract. Only a
		// Drain-context retry (this loop's own reaction-debt turn, which can
		// legitimately race a web tab mid-pull on the same session) keeps
		// retrying -- bounded overall, and visibly (stderr), never silently
		// forever.
		var lockBusy *session.SessionLockBusyError
		if errors.As(err, &lockBusy) {
			if firstTurn {
				return final, err
			}
			if lockBusyRetryStart.IsZero() {
				lockBusyRetryStart = time.Now()
			}
			if elapsed := time.Since(lockBusyRetryStart); elapsed > cliLockBusyRetryOverallLimit {
				fmt.Fprintf(os.Stderr, "rush run: session %q still locked by another process after %s; giving up\n",
					sessionID, cliLockBusyRetryOverallLimit)
				return final, err
			}
			fmt.Fprintf(os.Stderr, "rush run: session %q is locked by another process; retrying\n", sessionID)
			if !sleepOrCtxDone(ctx, cliLockBusyRetryPause) {
				return final, ctx.Err()
			}
			continue
		}
		lockBusyRetryStart = time.Time{}

		// A Drain iteration that ran no provider turn -- nothing wake-worthy
		// was pending (typically the notice was already pulled at a step
		// boundary of the previous turn), or it queued behind another owner
		// that pulls it itself -- surfaces as ErrRunQueued with an empty
		// envelope. Not a turn: it must neither replace the last turn's
		// result nor become the run's error.
		drainNoTurn := !firstTurn && errors.Is(err, ErrRunQueued)
		if !firstTurn && !drainNoTurn {
			source.RecordDrainTurnOutcome(ctx, sessionID, drainSnapshot, err)
		}
		if result != nil && !drainNoTurn {
			final = result
			sessionID = result.SessionID
			totalTokens += result.Usage.DeltaTokens
			totalCost += result.Usage.DeltaCostUSD
			warnings = append(warnings, result.Warnings...)
			subAgentOutputs = append(subAgentOutputs, result.SubAgentOutputs...)
			for _, stat := range result.ToolCalls {
				counts[stat.Name] += stat.Count
			}
		}
		if !drainNoTurn {
			runErr = err
			lastBuffered = buffered
		}
		if sessionID == "" || ctx.Err() != nil {
			return final, runErr
		}

		waitAfterNoTurnDrain(ctx, source, sessionID, drainNoTurn)

		// Doc sec.3.5: turn <=> the root's reaction debt (or the initial
		// request, already run above); exit <=> the root's scope is
		// closed. waitForNextCLITurn owns the DB predicate/wait/retry loop
		// entirely -- see its own doc.
		hasNext, waitErr := app.waitForNextCLITurn(ctx, source, sessionID)
		if waitErr != nil {
			if final != nil {
				final.ExitReason = "canceled"
				final.Error = waitErr.Error()
			}
			return final, waitErr
		}
		if !hasNext {
			if final == nil {
				return nil, runErr
			}
			final.Usage.DeltaTokens = totalTokens
			final.Usage.DeltaCostUSD = totalCost
			final.Warnings = warnings
			final.SubAgentOutputs = subAgentOutputs
			final.DurationMs = time.Since(started).Milliseconds()
			final.ToolCalls = final.ToolCalls[:0]
			for name, count := range counts {
				final.ToolCalls = append(final.ToolCalls, ToolCallStat{Name: name, Count: count})
			}
			slices.SortFunc(final.ToolCalls, func(a, b ToolCallStat) int { return cmpName(a.Name, b.Name) })
			if mode == RunModeTerse {
				if _, writeErr := io.Copy(output, lastBuffered); writeErr != nil {
					return final, writeErr
				}
			}
			if mode == RunModeJSON {
				if encErr := json.NewEncoder(output).Encode(final); encErr != nil {
					return final, fmt.Errorf("failed to encode JSON result: %w", encErr)
				}
			}
			return final, runErr
		}
		// Empty prompt: a Drain-kind turn (doc sec.3.4). Its own turn-start
		// pull moves whatever pending notice(s) constitute the debt into
		// history and reacts to it -- never to this loop's own (there is
		// none) captured text.
		prompt = ""
		continueSessionID = sessionID
		useLast = false
		firstTurn = false
	}
}

// waitAfterNoTurnDrain implements the B8/C5b,c fix (docs/reviews/2026-09-29-
// async-phase4-round1.md): a no-turn Drain (ErrRunQueued -- decideDrainTurn
// found no VISIBLE debt for a row waitForNextCLITurn's own pending-inclusive
// predicate just said WAS owed) must not loop straight back into an
// identical immediate re-check. If the row is still 'pending' because its
// pull keeps failing, the very next waitForNextCLITurn call would see the
// SAME pending debt and relaunch instantly again -- a 100%-CPU tight loop
// with no pause, never exiting. Waiting for a hint (or its own bounded
// fallback) first paces every SUBSEQUENT no-turn iteration without slowing
// down the common case at all: a genuinely fresh pending notice's FIRST
// Drain attempt pulls and reacts to it successfully (drainNoTurn is false),
// so this wait is never reached for it.
func waitAfterNoTurnDrain(ctx context.Context, source agent.ReactionDebtSource, sessionID string, drainNoTurn bool) {
	if drainNoTurn {
		source.WaitForHint(ctx, sessionID)
	}
}

// waitForNextCLITurn implements doc sec.3.5's CLI-loop predicate: another
// turn is owed iff sessionID's reaction debt exists right now; otherwise the
// loop waits (a hint, or a bounded same-process fallback tick) and
// re-evaluates, until the scope closes (exit, hasNext=false) or a debt
// appears (hasNext=true). A DB read error retries with a pause rather than
// silently treating the scope as open or closed. Only ctx cancellation ends
// the wait with an error.
func (app *App) waitForNextCLITurn(ctx context.Context, source agent.ReactionDebtSource, sessionID string) (hasNext bool, err error) {
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Pending-inclusive: a freshly-arrived notice is normally still
		// 'pending' here (nothing has pulled it into history yet) and MUST
		// still be reported as a turn owed -- the next turn's own preamble
		// pull is what moves it to 'done' and reacts to it. Using the
		// VISIBLE-only predicate here instead (an earlier version of the B8/
		// C5b,c fix) broke this common case: it is never visible until AFTER
		// a turn already pulled it, so this check saw no debt, fell through
		// to ScopeOpen, and blocked on WaitForHint's fallback for a hint that
		// had already fired before this call started watching for it --
		// several seconds of pure waste on every single async completion.
		// The actual tight-loop case (a permanently-pending row whose pull
		// keeps failing) is bounded below instead, at the one place that can
		// tell "this specific attempt already ran and found nothing to
		// react to" (drainNoTurn) without misclassifying a fresh notice.
		debt, debtErr := source.ReactionDebtExists(ctx, sessionID)
		if debtErr != nil {
			slog.Warn("rush run: reaction debt check failed; retrying", "session_id", sessionID, "err", debtErr)
			if !sleepOrCtxDone(ctx, cliDBRetryPause) {
				return false, ctx.Err()
			}
			continue
		}
		if debt {
			return true, nil
		}
		open, openErr := source.ScopeOpen(ctx, sessionID)
		if openErr != nil {
			slog.Warn("rush run: scope check failed; retrying", "session_id", sessionID, "err", openErr)
			if !sleepOrCtxDone(ctx, cliDBRetryPause) {
				return false, ctx.Err()
			}
			continue
		}
		if !open {
			return false, nil
		}
		// Scope is open on running work, not debt (e.g. a delegation still
		// armed, or a bash job still running) -- wait for a hint (or the
		// bounded same-process fallback inside WaitForHint) and re-check.
		// This is a same-process wait; the cross-process fallback is the
		// coordinator's own 60s pass re-evaluating parked delegations/
		// recheck-set sessions independently (doc sec.3.5).
		source.WaitForHint(ctx, sessionID)
	}
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
