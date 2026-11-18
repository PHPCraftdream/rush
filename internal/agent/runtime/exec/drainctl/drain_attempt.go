// Package-level drain pacing knobs and the settle-text tail, moved verbatim
// from package agent (coordinator_recheck.go / drain_component.go). The
// atomics are package-private here; package agent keeps small Load/Swap/Store
// adapters so existing root tests compile unchanged.
package drainctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

// recheckPassIntervalNS is the ticker period in nanoseconds, atomic so a test
// can drive the real ticker at test timescale while a never-stopped ticker of
// an earlier test exists; production never changes it.
var recheckPassIntervalNS atomic.Int64

func init() { recheckPassIntervalNS.Store(int64(60 * time.Second)) }

// drainRetryAfterNS paces the next launch after an unreacted attempt;
// drainRefusalPauseNS paces a refused launch for a session this process's own
// `rush run` loop drives (external driver) only. drainAccountBudgetNS bounds a
// whole accounting (its DB reads, the credential refresh and the settle);
// drainRefreshBudgetNS bounds the 401 credential refresh inside it, detached
// from the accounting's context so a black-holed auth endpoint cannot eat the
// settle's budget (R4B-3). Atomic so a test can shrink them at test timescale
// while a lingering goroutine of an earlier test reads.
var drainRetryAfterNS, drainRefusalPauseNS, drainAccountBudgetNS, drainRefreshBudgetNS atomic.Int64

func init() {
	drainRetryAfterNS.Store(int64(60 * time.Second))
	drainRefusalPauseNS.Store(int64(500 * time.Millisecond))
	drainAccountBudgetNS.Store(int64(30 * time.Second))
	drainRefreshBudgetNS.Store(int64(10 * time.Second))
}

// Narrow test-only accessors: package agent's adapters bind these so root
// tests keep touching the same knob without the atomics being exported.
func LoadRecheckPassIntervalNS() int64        { return recheckPassIntervalNS.Load() }
func SwapRecheckPassIntervalNS(v int64) int64 { return recheckPassIntervalNS.Swap(v) }
func StoreRecheckPassIntervalNS(v int64)      { recheckPassIntervalNS.Store(v) }

func LoadDrainRetryAfterNS() int64        { return drainRetryAfterNS.Load() }
func SwapDrainRetryAfterNS(v int64) int64 { return drainRetryAfterNS.Swap(v) }
func StoreDrainRetryAfterNS(v int64)      { drainRetryAfterNS.Store(v) }

func LoadDrainRefusalPauseNS() int64        { return drainRefusalPauseNS.Load() }
func SwapDrainRefusalPauseNS(v int64) int64 { return drainRefusalPauseNS.Swap(v) }
func StoreDrainRefusalPauseNS(v int64)      { drainRefusalPauseNS.Store(v) }

func LoadDrainAccountBudgetNS() int64        { return drainAccountBudgetNS.Load() }
func SwapDrainAccountBudgetNS(v int64) int64 { return drainAccountBudgetNS.Swap(v) }
func StoreDrainAccountBudgetNS(v int64)      { drainAccountBudgetNS.Store(v) }

func LoadDrainRefreshBudgetNS() int64        { return drainRefreshBudgetNS.Load() }
func SwapDrainRefreshBudgetNS(v int64) int64 { return drainRefreshBudgetNS.Swap(v) }
func StoreDrainRefreshBudgetNS(v int64)      { drainRefreshBudgetNS.Store(v) }

// DrainPacingForTest is the implementation behind agent.SetDrainPacingForTest.
func DrainPacingForTest(retryAfter, refusalPause, recheckInterval time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("agent.SetDrainPacingForTest called outside a test binary")
	}
	oldRetry, oldRefusal, oldRecheck := drainRetryAfterNS.Load(), drainRefusalPauseNS.Load(), recheckPassIntervalNS.Load()
	if retryAfter > 0 {
		drainRetryAfterNS.Store(int64(retryAfter))
	}
	if refusalPause > 0 {
		drainRefusalPauseNS.Store(int64(refusalPause))
	}
	if recheckInterval > 0 {
		recheckPassIntervalNS.Store(int64(recheckInterval))
	}
	return func() {
		drainRetryAfterNS.Store(oldRetry)
		drainRefusalPauseNS.Store(oldRefusal)
		recheckPassIntervalNS.Store(oldRecheck)
	}
}

const (
	// settleTailNextTurn: the events are in history and a later turn may react.
	settleTailNextTurn = "Событие сохранено; продолжение — при следующем ходе."
	// settleTailContextTooLarge: the events are in history and the history no
	// longer fits, so every next request of the session fails the same way until
	// it is shrunk (R8B-5).
	settleTailContextTooLarge = "Событие сохранено в истории, но она уже не помещается в окно контекста модели: следующие запросы этой сессии тоже будут отклонены. Сожмите историю (summarize) или начните новую сессию."
)

// contextOverflowMessage matches the providers that report an oversized prompt
// as a plain 400 (fantasy flags only the Anthropic and Google shapes).
var contextOverflowMessage = regexp.MustCompile(`(?i)prompt is too long|input is too long|context[_ ]length|maximum context|context window|too many tokens|reduce the length`)

// isContextOverflow reports whether err says the request did not fit the
// model's context window: a flagged fantasy error, a 413, or a 400 whose
// message names the overflow.
func isContextOverflow(err error) bool {
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	if providerErr.IsContextTooLarge() || providerErr.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	return providerErr.StatusCode == http.StatusBadRequest && contextOverflowMessage.MatchString(providerErr.Message)
}

// SettleTailFor picks the marker tail for the failure that closed the debt.
func SettleTailFor(err error) string {
	if isContextOverflow(err) {
		return settleTailContextTooLarge
	}
	return settleTailNextTurn
}

func RetryAfter() time.Duration    { return time.Duration(drainRetryAfterNS.Load()) }
func RefusalPause() time.Duration  { return time.Duration(drainRefusalPauseNS.Load()) }
func AccountBudget() time.Duration { return time.Duration(drainAccountBudgetNS.Load()) }
func RefreshBudget() time.Duration { return time.Duration(drainRefreshBudgetNS.Load()) }

type DrainOutcome uint8

const (
	DrainNotAttempted DrainOutcome = iota
	DrainNoTurn
	DrainAttempted
)

func OperatorStop(err error, turnCtxDone bool) bool {
	return errors.Is(err, context.Canceled) || (turnCtxDone && errors.Is(err, context.DeadlineExceeded))
}

func DrainFailureTerminal(err error, turnCtxDone bool, peakHours error, terminalClass func(error) bool) bool {
	if err == nil || errors.Is(err, peakHours) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) && !turnCtxDone {
		return false
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	return terminalClass(err)
}

func ProviderTurnFailed(err error, turnCtxDone bool, peakHours error, classifiable func(error) bool) bool {
	if OperatorStop(err, turnCtxDone) || errors.Is(err, peakHours) {
		return false
	}
	return classifiable(err)
}

type RetryClass int

const (
	// ClassTerminal is an operator-actionable failure (quota wall, auth,
	// context overflow, bad request, user cancel) that must surface.
	ClassTerminal RetryClass = iota
	// ClassTransient is a provider/network hiccup worth a re-run.
	ClassTransient
	// ClassRateLimit is a 429 non-quota overload: retryable with its own
	// dedicated wait budget.
	ClassRateLimit
)

// Seams injects the agent-package predicates ClassifyProviderError needs.
// HardProviderLimit wraps the hard-provider-limit check (billing walls some
// providers serve as 5xx) with the original call shape intact.
type Seams struct {
	ReplayForbidden   func(error) bool
	HardProviderLimit func(error) bool
}

// ClassifyProviderError classifies a NON-NIL turn-terminating error.
// context cancellation is terminal here — watchdog stalls are matched
// separately by their persisted finish title in shouldRetryTurn, because
// a stall surfaces only as context.Canceled.
func ClassifyProviderError(err error, peakHours error, seams Seams) RetryClass {
	if seams.ReplayForbidden(err) {
		return ClassTerminal
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassTerminal
	}
	// Peak-hours refusal is operator policy, not a transient hiccup: the
	// condition only clears when the wall clock leaves the window, so a
	// backoff retry would just burn the backoff and fail identically.
	if errors.Is(err, peakHours) {
		return ClassTerminal
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.IsContextTooLarge() {
			return ClassTerminal // the auto-summarize path owns this
		}
		switch providerErr.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired:
			return ClassTerminal
		case http.StatusForbidden:
			// 403 is ambiguous: it can be a real auth/geo wall (retry pointless)
			// or a CDN/anti-abuse banner from a fronting balancer that clears in
			// tens of seconds (z.ai "Forbidden ZS", Cloudflare-fronted providers).
			// Treat as transient: the worst case is ~40s of bounded backoff on a
			// truly bad key vs. losing a long agent run on a momentary block.
			return ClassTransient
		case http.StatusTooManyRequests:
			if usage.IsQuotaLimit(providerErr) {
				return ClassTerminal // multi-hour usage wall — operator accepts a fast fail
			}
			return ClassRateLimit // momentary overload — dedicated wait budget
		case http.StatusRequestTimeout, http.StatusConflict:
			return ClassTransient
		}
		if providerErr.StatusCode >= 500 {
			// A billing wall some providers (MiniMax 1008) serve as a 5xx never clears.
			if seams.HardProviderLimit(err) {
				return ClassTerminal
			}
			return ClassTransient
		}
		if providerErr.StatusCode >= 400 {
			return ClassTerminal // genuine client error (400, 404, ...)
		}
		// No HTTP status (status 0): EOF / network wrapped as ProviderError.
		if providerErr.IsRetryable() {
			return ClassTransient
		}
		return ClassTerminal
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ClassTransient
	}
	return ClassTerminal
}

// IsProviderClassifiable reports whether err is a shape ClassifyProviderError
// can reason about as an ACTUAL provider-turn outcome (a provider error, a
// network error, or the peak-hours refusal) -- as opposed to some other
// error a Drain's turn can return before ever reaching the provider (e.g. a
// DB read failure in the turn-start preamble). Used by
// settleOrRetryDrainFailure (coordinator_drain_policy.go) as its fallback
// classifier when no per-attempt assistant-message evidence is available
// (docs/reviews/2026-09-29-async-phase4-round1.md B5/C3, W-DRAIN item 1):
// treating every unrecognized error as ClassifyProviderError's terminal
// default would close debt and write a visible marker for a failure that
// never touched the provider at all.
func IsProviderClassifiable(err error, peakHours error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, peakHours) {
		return true
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func ForeignWorkspaceError(sess coresession.Session) error {
	msg := fmt.Sprintf(
		"session %s belongs to %s (branch %s); run it there or \"rush sessions fork %s\" here",
		sess.ID, sess.WorkspaceRoot, sess.GitBranch, sess.ID,
	)
	if sess.WorkspaceRoot != "" {
		if _, err := os.Stat(sess.WorkspaceRoot); err != nil {
			// The owning checkout is gone (`git worktree remove`): say so, or
			// the operator goes hunting for a directory that no longer
			// exists. A legacy row's empty root is never stat-ed -- it names no
			// directory to be missing.
			msg += " (workspace no longer exists)"
		}
	}
	return fmt.Errorf("%s: %w", msg, runq.ErrForeignWorkspace)
}
