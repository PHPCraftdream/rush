// Package-level drain pacing knobs and the settle-text tail, moved verbatim
// from package agent (coordinator_recheck.go / drain_component.go). The
// atomics are package-private here; package agent keeps small Load/Swap/Store
// adapters so existing root tests compile unchanged.
package drainctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/session"
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

// settleTailFor picks the marker tail for the failure that closed the debt.
func settleTailFor(err error) string {
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

// Drain attempt accounting (moved verbatim from package agent's
// drainComponent, drain_component.go): the root package keeps the private
// drainAttempt record and the thin coordinator delegates; this file owns the
// title-level accounting itself, written against the narrow seams below --
// no root types appear here.

// AttemptPort is the narrow subset of the root BatchCutoverFacts surface the
// accounting reads and writes, at use time (the port is fetched per use, so
// the source is selected where the original code called batchCutoverFacts).
type AttemptPort interface {
	// DebtToolCallIDs resolves each snapshot ref's root-row tool_call_id
	// (the chain identity vocabulary / the settle marker text).
	DebtToolCallIDs(ctx context.Context, snap session.BatchDebtSnapshot) []string
	// IncrementWakeAttempts counts one unreacted attempt on every row of
	// the acted-on snapshot.
	IncrementWakeAttempts(ctx context.Context, owner string, snap session.BatchDebtSnapshot) error
	// AttemptRows rereads the GIVEN acted-on snapshot after an attempt was
	// counted (per-row wake attempts against the atK threshold).
	AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (AttemptRows, error)
	// SettleReactedFailedWithMarker closes EXACTLY the snapshot's rows by
	// failure and writes the wake_failed marker; returns the settled count.
	SettleReactedFailedWithMarker(ctx context.Context, owner string, snap session.BatchDebtSnapshot, text string) (int64, error)
}

// AttemptRows is the post-attempt per-row picture of the visible debt (the
// A5/A6 inputs); the root's BatchAttemptRows projects onto it.
type AttemptRows struct {
	Open        int
	Max         int
	AtK         session.BatchDebtSnapshot
	UnmappedAtK int
}

// AttemptGovernor is the gate/chain/auth surface the accounting acts on
// (the arbiter state's writers, R-ARB-2, plus the 401 predicates).
type AttemptGovernor interface {
	// AccountFacts is the accounting port, fetched at use time (the original
	// code constructed batchCutoverFacts at each use site).
	AccountFacts() AttemptPort
	// Now is the arbiter's clock.
	Now() time.Time
	// Pace writes the launch gate (arbiter pace); it reports whether the
	// write turned the gate dormant.
	Pace(sid string, hintAt uint64, wait time.Duration, hintOpens bool, kind Pace) (becameDormant bool)
	// ChainLink records the reaction-chain link of the attempt's leg; the
	// governor side reads the attempt record itself.
	ChainLink(sid string, snap session.BatchDebtSnapshot)
	// ResetGate opens the launch gate.
	ResetGate(sid string)
	// AddToRecheckSet queues the session for a re-check tick.
	AddToRecheckSet(sid string)
	// IsExternalDriver reports whether the session's drain launches are
	// driven by this process's own `rush run` loop.
	IsExternalDriver(sid string) bool
	// HintSeqOf reads the current driver-hint counter.
	HintSeqOf(sid string) uint64
	// IsUnauthorized reports a 401-shaped provider error.
	IsUnauthorized(err error) bool
	// RefreshAfterUnauthorized refreshes providerID's credentials.
	RefreshAfterUnauthorized(ctx context.Context, providerID string) bool
	// PublishReauthenticate asks the operator to re-authenticate.
	PublishReauthenticate(providerID string)
	// RedactURLs rewrites URL-looking tokens in an error text (R2-5).
	RedactURLs(s string) string
}

// AttemptRecord is the explicit projection of the root drainAttempt record
// the accounting reads and writes. The atomic flags are read through
// function values AT USE TIME, in the original evaluation order; the
// setters write back exactly what the original assigned on the record.
type AttemptRecord struct {
	SessionID   string
	HintAt      uint64
	Outcome     DrainOutcome
	PendingLeft bool
	// CommitNoRecheck is the no-turn commit verdict's recheck question
	// (att.commitNo.recheck).
	CommitNoRecheck bool
	// StreamErr is the error agent.Stream returned (read once, up front,
	// as the leg's failure when the turn loop reported none).
	StreamErr error
	// Stalled / CapAbort / TurnCtxDone read the record's atomics at use time.
	Stalled      func() bool
	CapAbort     func() bool
	TurnCtxDone  func() bool
	Provider     string
	Credentialed bool
	// BatchSnapshot is the acted-on set captured at the turn-start pull.
	BatchSnapshot session.BatchDebtSnapshot
	BatchCaptured bool
	// SetBatchDebtCallIDs assigns the root-call-id vocabulary onto the
	// record BEFORE the reaction chain guard is invoked (order is law).
	SetBatchDebtCallIDs func(ids []string)
}

// attemptFailure is the error the leg ended with: the turn loop's own error,
// else the one agent.Stream returned (a cancelled leg handed to the next
// queued call reports nil).
func attemptFailure(rec AttemptRecord, turnErr error) error {
	if turnErr != nil {
		return turnErr
	}
	return rec.StreamErr
}

// attemptExempt: the attempt ended for a reason that is not evidence about
// the debt (operatorStop). A watchdog stall and a cap abort surface as
// a cancellation too but ARE real, paid attempts.
func attemptExempt(rec AttemptRecord, err error) bool {
	if rec.Stalled() || rec.CapAbort() {
		return false
	}
	return OperatorStop(err, rec.TurnCtxDone())
}

// AccountAttempt is THE accounting function of a Drain leg. The DECISION is
// DecideAccount (the accounting rows A1-A7 of the arbiter table); this
// function gathers the attempt's facts, asks the arbiter, and executes the
// verdict -- the gate/streak/chain writes themselves are the governor's
// (R-ARB-2). The DB failure paths (a failed count, a failed snapshot read, a
// failed settle) keep today's behavior: pace the gate like an unreacted paid
// attempt and stay in the recheck set. failureTerminal is the root's
// use-time failure-classification callback (drainFailureTerminal): the
// peak-hours sentinel and the provider-error predicates are read only when
// the classification actually runs, not before the DB callbacks.
func AccountAttempt(ctx context.Context, gov AttemptGovernor, rec AttemptRecord, turnErr error, failureTerminal func(error, bool) bool) {
	if rec.SessionID == "" {
		return
	}
	sid := rec.SessionID
	turnErr = attemptFailure(rec, turnErr)
	turnCtxDone := rec.TurnCtxDone()
	facts := TurnFacts{Now: gov.Now(), Site: SiteAccount}
	facts.Session.ExternallyDrivenSelf = gov.IsExternalDriver(sid)
	// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1
	// "drain/reaction"): the leg's error is the ONLY place the run-queue/
	// runOwned ownership refusal is visible from here -- the DB half of the
	// facts holds no workspace column. Stamping it here is what makes
	// DecideAccount defer that leg without pacing or settling anything.
	facts.Session.ForeignWorkspace = errors.Is(turnErr, runq.ErrForeignWorkspace)
	switch rec.Outcome {
	case DrainNotAttempted:
		if turnErr == nil || OperatorStop(turnErr, turnCtxDone) {
			return // a cancelled preamble: nothing was refused, nothing was tried
		}
		facts.Attempt = AttemptFacts{NotAttempted: true, Refused: true}
		executeAccountVerdict(ctx, gov, rec, turnErr, DecideAccount(facts, RetryAfter, RefusalPause), 0)
		return
	case DrainNoTurn:
		// R12-E fix: with the batch runtime on, the pull-time capture fills
		// the record's batch snapshot and leaves the legacy snapshot zero
		// (agent_turn.go), so visible batch debt is exactly a nonempty batch
		// snapshot. Without this term a flag-ON policy refusal reported
		// CommitRefused=false and fell through to "no debt: gate reset",
		// clearing a gate the attempt produced no evidence about. A failed
		// capture cannot reach the no-turn path: the ON capture either fails
		// the turn (error return) or runs uncounted committed (the !commit
		// branch is skipped for DrainTurnCommitted), so batchCaptured true
		// here means the capture succeeded.
		batchRefused := rec.BatchCaptured && !rec.BatchSnapshot.Empty()
		facts.Attempt = AttemptFacts{NoTurn: true, PendingLeft: rec.PendingLeft, CommitRefused: batchRefused}
		executeAccountVerdict(ctx, gov, rec, turnErr, DecideAccount(facts, RetryAfter, RefusalPause), 0)
		return
	}
	if attemptExempt(rec, turnErr) {
		return
	}
	pace := func() { paceUnreacted(gov, sid, rec.HintAt, false, PacePaidUnreacted) }
	if turnErr == nil {
		// Reaction chain guard (#1113): account the leg's link BEFORE the
		// debt's post-attempt state is read -- a successful reaction (rows.
		// open == 0 below) must not reset the count. On the batch path a leg
		// whose pull-time capture failed is as undocumented as a failed
		// legacy snapshot read: skip the link too (same conservatism as the
		// accounting below, which will not count it). The batch guard
		// compares ROOT tool_call_ids, so the acted-on rows are resolved
		// into the identity vocabulary first.
		bfacts := gov.AccountFacts()
		if !rec.BatchCaptured {
			slog.Warn("drain attempt: pull-time batch debt capture missing; link unaccounted",
				"session_id", sid)
			pace()
			return
		}
		rec.SetBatchDebtCallIDs(bfacts.DebtToolCallIDs(ctx, rec.BatchSnapshot))
		gov.ChainLink(sid, rec.BatchSnapshot)
	}
	// Count and reread through the ACTED-ON snapshot captured at
	// the turn-start pull (the record's batch snapshot). A leg without that
	// capture is not counted and settles nothing -- a late recapture would
	// widen the acted-on set with rows that arrived after the pull.
	{
		bfacts := gov.AccountFacts()
		if !rec.BatchCaptured {
			slog.Warn("drain attempt: no pull-time batch debt snapshot; attempt uncounted",
				"session_id", sid)
			pace()
			return
		}
		snap := rec.BatchSnapshot
		if err := bfacts.IncrementWakeAttempts(ctx, sid, snap); err != nil {
			slog.Error("drain attempt: increment wake attempts failed", "session_id", sid, "err", err)
			pace()
			return
		}
		batchRows, err := bfacts.AttemptRows(ctx, sid, snap, FailureSettleThreshold)
		if err != nil {
			slog.Error("drain attempt: read wake attempts failed", "session_id", sid, "err", err)
			pace()
			return
		}
		facts.Attempt = AttemptFacts{
			MaxWakeAttempts: batchRows.Max,
			OpenRows:        batchRows.Open,
			AtKBatch:        batchRows.AtK,
			UnmappedAtK:     batchRows.UnmappedAtK,
			Terminal:        attemptTerminal(ctx, gov, rec, turnErr, turnCtxDone, failureTerminal),
		}
		facts.BatchSnapshot = snap
		executeAccountVerdict(ctx, gov, rec, turnErr, DecideAccount(facts, RetryAfter, RefusalPause), batchRows.Open)
	}
}

// executeAccountVerdict executes DecideAccount's verdict on the real state:
// pacing, resets and the settle-by-failure. openRows is the post-attempt
// count of still-open snapshot rows (the VClose executor keeps the retry
// pace for the rows it did not close).
func executeAccountVerdict(ctx context.Context, gov AttemptGovernor, rec AttemptRecord, turnErr error, v Verdict, openRows int) {
	sid := rec.SessionID
	pacePaid := func() { paceUnreacted(gov, sid, rec.HintAt, false, PacePaidUnreacted) }
	switch {
	case v.Kind == VDefer && v.Reason == "launch refused":
		// A1: the admission refusal handler paces uncounted and rechecks.
		NoteDrainRefused(gov, sid, turnErr)
	case v.Kind == VDefer && v.Reason == "pending debt left behind":
		// A2: a no-turn Drain over a pull that keeps failing (free streak).
		paceUnreacted(gov, sid, rec.HintAt, true, PaceFreeNoTurn)
	case v.Kind == VClose:
		// A6: settle exactly the named rows by failure, with the visible
		// marker; rows with fewer attempts keep the retry pace.
		cause := "the assistant responded, but its reaction to this event was not recorded"
		if turnErr != nil {
			cause = gov.RedactURLs(turnErr.Error())
		}
		if bfacts := gov.AccountFacts(); !v.BatchRows.Empty() {
			// The settle closes EXACTLY the verdict's rows. The marker names
			// the acted-on tool-call ids.
			ids := bfacts.DebtToolCallIDs(ctx, v.BatchRows)
			trimmed := make([]string, 0, len(ids))
			for _, id := range ids {
				id, _, _ := strings.Cut(id, "#reused#")
				trimmed = append(trimmed, id)
			}
			var text string
			if len(trimmed) > 0 {
				text = fmt.Sprintf(
					"Не удалось продолжить работу после события %s: %s. %s",
					strings.Join(trimmed, ", "), cause, settleTailFor(turnErr))
			} else {
				text = fmt.Sprintf(
					"Не удалось продолжить работу после ошибки провайдера: %s. %s",
					cause, settleTailFor(turnErr))
			}
			if _, err := bfacts.SettleReactedFailedWithMarker(ctx, sid, v.BatchRows, text); err != nil {
				pacePaid()
				return
			}
			gov.ResetGate(sid)
			closed := len(v.BatchRows.Nodes) + len(v.BatchRows.Notices)
			if closed < openRows {
				pacePaid()
			}
			return
		}
	case v.Kind == VDefer && v.Reason == ForeignWorkspaceReason:
		// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1
		// "drain/reaction"): the leg was refused because the session belongs
		// to another workspace, so NOTHING here is this process's to write.
		// The gate keeps its state (a refusal that says nothing about our own
		// launch cadence must not pace it), nothing is settled (the debt is
		// the owner's to answer), and no recheck is queued (the reason is
		// permanent, not transient). Falling through to A7's generic VDefer
		// would shut our gate over a session we have no claim on.
	case v.Kind == VDefer:
		// A7: a counted, unreacted attempt: the gate paces at R and a newer
		// fact does not open it early (R3B-4).
		pacePaid()
	case v.Reason == "commit refused: gate untouched":
		// A3': the launch verdict refused the commit while visible debt
		// stays; the gate is the launch side's state, not this attempt's
		// evidence -- it is neither opened nor closed. Only the recheck
		// question survives (the old default branch).
		if rec.CommitNoRecheck {
			gov.AddToRecheckSet(sid)
		}
	case v.Reason == "no debt: gate reset" || v.Reason == "all rows reacted: gate reset":
		// A3/A5: nothing owed (or every visible row reacted): the gate opens.
		gov.ResetGate(sid)
	}
}

// attemptTerminal reports whether the attempt's failure closes every row
// it saw at once: a provider classification no retry can fix. A 401 is
// terminal only when the credentials cannot be refreshed: a refresh that
// works (an OAuth token that expired during a long job) makes it a counted,
// paced transient, because the next attempt runs on the new credentials. A
// 401 that stays terminal asks the operator to re-authenticate (hyper).
func attemptTerminal(ctx context.Context, gov AttemptGovernor, rec AttemptRecord, turnErr error, turnCtxDone bool, failureTerminal func(error, bool) bool) bool {
	if !failureTerminal(turnErr, turnCtxDone) {
		return false
	}
	if !gov.IsUnauthorized(turnErr) {
		return true
	}
	if !rec.Credentialed && gov.RefreshAfterUnauthorized(ctx, rec.Provider) {
		return false
	}
	gov.PublishReauthenticate(rec.Provider)
	return true
}

// NoteDrainRefused is the one handler of an admission refusal (lock busy or
// unwritable, model persist failure, provider not configured, peak hours,
// shutdown, an unreadable pre-turn DB read). A refusal is neither counted nor
// settled: the debt stays and the launch is retried -- at once (every 0.5s)
// by a `rush run` loop that drives the session itself, at the next re-check
// tick otherwise. A newer fact opens the gate early.
func NoteDrainRefused(gov AttemptGovernor, sessionID string, cause error) {
	if sessionID == "" {
		return
	}
	external := gov.IsExternalDriver(sessionID)
	wait := RetryAfter()
	if external {
		wait = RefusalPause()
	}
	gov.Pace(sessionID, gov.HintSeqOf(sessionID), wait, true, PaceUncounted)
	if !external {
		gov.AddToRecheckSet(sessionID)
	}
	slog.Debug("drain launch refused; the debt stays", "session_id", sessionID, "err", cause)
}

// paceUnreacted shuts the gate after an unreacted outcome and queues the
// session for a re-check tick. The moment a streak turns the gate dormant it
// says so once, naming what reopens it: the debt stays visible (`sessions
// why`). A pull that keeps failing (free) is reopened by a newer event or a
// human message; paid attempts whose close kept failing only by a human
// message or a restart.
func paceUnreacted(gov AttemptGovernor, sessionID string, hintAt uint64, hintOpens bool, kind Pace) {
	if gov.Pace(sessionID, hintAt, RetryAfter(), hintOpens, kind) {
		if kind == PaceFreeNoTurn {
			slog.Warn("drain launches for this session are paused: the notice pull keeps failing; a new event or a human message resumes them",
				"session_id", sessionID)
		} else {
			slog.Warn("drain launches for this session are paused: repeated attempts could not be closed or counted; only a human message (or a restart) resumes them",
				"session_id", sessionID)
		}
	}
	gov.AddToRecheckSet(sessionID)
}
