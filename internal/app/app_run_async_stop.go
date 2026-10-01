package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/session"
)

// stopError is the run's end by its own limits or by the operator (nil when
// neither applies): --max-cost compares the SUBTREE BUDGET (#1130: own +
// delegation children, minus the reset base — the same query the agent's
// enforceRunawayCaps runs), --max-tokens compares the root row's token
// snapshots (per-node counters, not tree-summed). Both are read before every
// paid turn and at every wake of a wait on running work or on the launch
// gate (at least every 5s), so a crossed cap or a cancel takes effect within
// one step, not when the awaited work ends (R7C-1, R8B-3): a delegated child
// charges its own cost_self the moment it spends, and the next budget read
// sees it. The exit goes through exitPrecheck: envelope, ended_reason,
// --on-finish, and the caller's Shutdown cancels the run's jobs as Ctrl-C does.
func (l *cliLoop) stopError() error {
	if l.overrides.MaxCost > 0 || l.overrides.MaxTokens > 0 {
		if sess, err := l.app.Sessions.Get(l.ctx, l.sessionID); err == nil {
			if capErr := l.capExceeded(sess); capErr != nil {
				return capErr
			}
			if sess.CancelRequested {
				return l.cancelRequestedError()
			}
			return nil
		}
	}
	return l.cancelError()
}

// cancelError is the run's end by `sessions cancel` (nil when none is pending).
func (l *cliLoop) cancelError() error {
	if canceled, err := l.app.Sessions.IsCancelRequested(l.ctx, l.sessionID); err == nil && canceled {
		return l.cancelRequestedError()
	}
	return nil
}

func (l *cliLoop) cancelRequestedError() error {
	return &runIncompleteError{reason: "canceled", detail: fmt.Sprintf("session %s cancelled by user", l.sessionID)}
}

// capError reports the run's --max-cost/--max-tokens as exceeded, comparing
// the same quantities as the agent's enforceRunawayCaps (subtree budget for
// cost, root-row snapshots for tokens). A single-step turn that crosses a
// cap is not cut short (fantasy ignores the step callback's error), so the
// loop checks after every Drain too.
func (l *cliLoop) capError() error {
	if l.overrides.MaxCost <= 0 && l.overrides.MaxTokens <= 0 {
		return nil
	}
	sess, err := l.app.Sessions.Get(l.ctx, l.sessionID)
	if err != nil {
		return nil
	}
	return l.capExceeded(sess)
}

func (l *cliLoop) capExceeded(sess session.Session) error {
	if l.overrides.MaxCost > 0 {
		if budget, err := l.app.Sessions.SubtreeBudget(l.ctx, l.sessionID); err == nil && budget > l.overrides.MaxCost {
			return &runIncompleteError{reason: "error", detail: fmt.Sprintf(
				"session %s aborted: cost $%.4f exceeds max $%.4f", l.sessionID, budget, l.overrides.MaxCost)}
		}
		// A failed budget read is not an abort: like stopError's snapshot
		// read, a transient DB error is no evidence the cap is crossed.
	}
	if total := sess.PromptTokens + sess.CompletionTokens; l.overrides.MaxTokens > 0 && total > l.overrides.MaxTokens {
		return &runIncompleteError{reason: "error", detail: fmt.Sprintf(
			"session %s aborted: %d tokens exceeds max %d", l.sessionID, total, l.overrides.MaxTokens)}
	}
	return nil
}

// exitedCanceled reports that the run is ending "canceled": the envelope says
// so, or the error is an operator cancel, Ctrl-C or a deadline.
func exitedCanceled(final *RunResult, err error) bool {
	if final != nil && (final.ExitReason == "canceled" || final.ExitReason == "cancelled") {
		return true
	}
	var inc *runIncompleteError
	if errors.As(err, &inc) && (inc.reason == "canceled" || inc.reason == "cancelled") {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// clearHonouredCancel makes the operator's cancel request one-shot on the way
// out (R8A-2): when the run ends "canceled" the request has been honoured, and
// a flag left behind would abort the next web turn or takeover Drain on the
// session after its first step. It runs before the exit flush, on a context
// detached from the run's, best effort. A run that ends for any other reason
// (a refused first turn included) honoured nothing and leaves the flag to
// whoever owns it; Drain turns never clear it (TestRunNonInteractive_
// DrainKeepsCancelRequest), so the loop still sees a request that lands mid-Drain.
func (l *cliLoop) clearHonouredCancel(final *RunResult, err error) {
	if l.sessionID == "" || !exitedCanceled(final, err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	if pending, readErr := l.app.Sessions.IsCancelRequested(ctx, l.sessionID); readErr != nil || !pending {
		return
	}
	if clearErr := l.app.Sessions.ClearCancelRequest(ctx, l.sessionID); clearErr != nil {
		slog.Warn("Failed to clear the honoured cancel request at loop exit", "session_id", l.sessionID, "err", clearErr)
	}
}
