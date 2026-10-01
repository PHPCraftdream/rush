// Drain attempt accounting (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md): ONE accounting point for a Drain leg, run by the turn
// loop that ran it -- exactly once, after the leg ends and before the mailbox
// is released -- from the DB's own verdict (are the rows this attempt could
// see still unreacted?). Launchers never account: a queued or merged Drain has
// no launcher, and the release hook fires inside Run before a launcher
// regains control. Launch decisions read the per-session gate this file
// writes (work_ledger_reaction.go, coordinator_drain_policy.go).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/session"
)

// drainOutcome says how far a Drain leg got.
type drainOutcome uint8

const (
	// drainNotAttempted: refused or failed in the preamble, before the provider.
	drainNotAttempted drainOutcome = iota
	// drainNoTurn: admitted, the commit decision said no.
	drainNoTurn
	// drainAttempted: agent.Stream was called.
	drainAttempted
)

// drainFailureSettleThreshold is K: a row whose OWN counter reaches this many
// counted, unreacted attempts is closed by failure (other rows keep theirs).
const drainFailureSettleThreshold = 3

// drainDormantStreak is the length of either dormancy streak (the arbiter gate):
// consecutive no-turn Drains over a failing pull (a newer fact reopens the
// gate) or consecutive paid attempts whose close kept failing (only a human
// message or a restart reopens it).
const drainDormantStreak = 3

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

func drainRetryAfterFailure() time.Duration { return time.Duration(drainRetryAfterNS.Load()) }
func drainRefusalPauseLoop() time.Duration  { return time.Duration(drainRefusalPauseNS.Load()) }
func drainAccountBudget() time.Duration     { return time.Duration(drainAccountBudgetNS.Load()) }
func drainRefreshBudget() time.Duration     { return time.Duration(drainRefreshBudgetNS.Load()) }

// ErrDrainNotAttempted marks a Drain that never reached the provider
// (refusal or a preamble failure); see DrainNotAttemptedError.
var ErrDrainNotAttempted = errors.New("drain turn not attempted")

// DrainNotAttemptedError wraps the refusal a Drain returned without having
// reached the provider. errors.Is(err, ErrDrainNotAttempted) is true and the
// wrapped error stays reachable through Unwrap.
type DrainNotAttemptedError struct{ Err error }

func (e *DrainNotAttemptedError) Error() string {
	if e.Err == nil {
		return ErrDrainNotAttempted.Error()
	}
	return e.Err.Error()
}
func (e *DrainNotAttemptedError) Unwrap() error { return e.Err }
func (e *DrainNotAttemptedError) Is(target error) bool {
	return target == ErrDrainNotAttempted
}

// IsDrainNotAttempted reports whether err is a Drain that never reached the
// provider.
func IsDrainNotAttempted(err error) bool { return errors.Is(err, ErrDrainNotAttempted) }

// notAttempted wraps err for a Drain call that never reached the provider;
// any other call kind keeps its error as is.
func notAttempted(call SessionAgentCall, err error) error {
	if err == nil || !call.IsDrain || IsDrainNotAttempted(err) {
		return err
	}
	return &DrainNotAttemptedError{Err: err}
}

// drainAttempt is one Drain leg (one turn-loop iteration of a Drain call).
type drainAttempt struct {
	sessionID string
	// hintAt is the hint counter read BEFORE the turn-start pull: a fact that
	// lands during the attempt is never absorbed by it.
	hintAt uint64
	// snapshot is the visible debt AFTER this Drain's own pull.
	snapshot session.DebtSnapshot
	outcome  drainOutcome
	// pendingLeft (no-turn): pending-inclusive debt remains -- the pull fails.
	pendingLeft bool
	// commitNo (no-turn with visible debt): the policy/gate verdict.
	commitNo drainVerdict
	// stalled: the stream watchdog fired (surfaces as context.Canceled).
	stalled atomic.Bool
	// capAbort: --max-cost/--max-tokens ended the turn (surfaces as Canceled).
	capAbort atomic.Bool
	// turnCtxDone: the turn's OWN context was done (cancelled or past its
	// deadline) when its error surfaced. A deadline error is an operator stop
	// only when this is true: net/http timeouts satisfy
	// errors.Is(err, context.DeadlineExceeded) too.
	turnCtxDone atomic.Bool
	// streamErr is the error agent.Stream returned: runTurn hands a cancelled
	// leg to the next queued call with a nil error, so the accounting reads
	// the cause here.
	streamErr error
	// provider is the provider id of the model the attempt called, and
	// credentialed says the call carries its own per-call credentials (which a
	// refresh of the shared config cannot repair): set just before the stream,
	// read by the 401 handling of the accounting.
	provider     string
	credentialed bool
	// chainIdleClaims holds the claim ids of the pure wait commands (bash
	// sleep/echo, run_command sleep/timeout) THIS leg launched async; each
	// completion becomes a debt row keyed by its claim. chainProgress says
	// the leg also did a real action (edit, a bash command with substance,
	// a delegation, ...). Written by recordStepHistory, read by the reaction
	// chain guard's accounting (#1113).
	chainIdleClaims []string
	chainProgress   bool
	closed          bool
}

// newDrainAttempt starts the accounting record of a Drain call's leg; nil for
// every other call kind. Called before the leg's turn-start pull.
func (a *sessionAgent) newDrainAttempt(call SessionAgentCall) *drainAttempt {
	if !call.IsDrain {
		return nil
	}
	att := &drainAttempt{sessionID: call.SessionID}
	if a.asyncJobs != nil {
		att.hintAt = a.asyncJobs.hintSeqOf(call.SessionID)
	}
	return att
}

// closeDrainAttempt accounts att exactly once (idempotent; nil-safe). It runs
// on a context detached from the turn: a cancelled turn's accounting is
// decided by drainAttemptExempt, never by the cancellation itself.
func (a *sessionAgent) closeDrainAttempt(att *drainAttempt, turnErr error) {
	if att == nil || att.closed {
		return
	}
	att.closed = true
	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
	defer cancel()
	a.asyncJobs.coord.accountDrainAttempt(ctx, att, turnErr)
}

// failure is the error the leg ended with: the turn loop's own error, else
// the one agent.Stream returned (a cancelled leg handed to the next queued
// call reports nil).
func (att *drainAttempt) failure(turnErr error) error {
	if turnErr != nil {
		return turnErr
	}
	return att.streamErr
}

// operatorStop reports whether err is the turn's context ending under an
// operator (Stop, shutdown, Ctrl-C, --timeout, interrupt/replace, `sessions
// cancel`), not a provider failure. A cancellation always is (only a
// cancelled context produces it); a deadline only when the TURN's own context
// hit it: a net/http timeout satisfies errors.Is(err, context.DeadlineExceeded)
// with a live turn context and is a transient provider failure.
func operatorStop(err error, turnCtxDone bool) bool {
	return errors.Is(err, context.Canceled) || (turnCtxDone && errors.Is(err, context.DeadlineExceeded))
}

// drainAttemptExempt: the attempt ended for a reason that is not evidence
// about the debt (operatorStop). A watchdog stall and a cap abort surface as
// a cancellation too but ARE real, paid attempts.
func drainAttemptExempt(att *drainAttempt, err error) bool {
	if att.stalled.Load() || att.capAbort.Load() {
		return false
	}
	return operatorStop(err, att.turnCtxDone.Load())
}

// drainFailureTerminal reports whether err is a provider failure no retry
// can fix (401/402/quota/other 4xx/context too large). Peak hours is a
// window, not a terminal failure, and neither is a transport timeout of a
// live turn (turnCtxDone false).
func drainFailureTerminal(err error, turnCtxDone bool) bool {
	if err == nil || errors.Is(err, errProviderPeakHours) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) && !turnCtxDone {
		return false
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) {
		return false
	}
	return classifyProviderError(err) == classTerminal
}

// accountDrainAttempt is THE accounting function of a Drain leg. The
// DECISION is decideAccount (the accounting rows A1-A7 of the arbiter
// table); this function gathers the attempt's facts, asks the arbiter, and
// executes the verdict -- the gate/streak/chain writes themselves are the
// arbiter state's writers (R-ARB-2). The DB failure paths (a failed count, a
// failed snapshot read, a failed settle) keep today's behavior: pace the
// gate like an unreacted paid attempt and stay in the recheck set.
func (c *coordinator) accountDrainAttempt(ctx context.Context, att *drainAttempt, turnErr error) {
	l := c.asyncJobs
	if l == nil || att == nil || att.sessionID == "" {
		return
	}
	sid := att.sessionID
	turnErr = att.failure(turnErr)
	turnCtxDone := att.turnCtxDone.Load()
	facts := TurnFacts{Now: time.Now(), Site: siteAccount}
	facts.Session.ExternallyDrivenSelf = l.isExternalDriver(sid)
	switch att.outcome {
	case drainNotAttempted:
		if turnErr == nil || operatorStop(turnErr, turnCtxDone) {
			return // a cancelled preamble: nothing was refused, nothing was tried
		}
		facts.Attempt = AttemptFacts{NotAttempted: true, Refused: true}
		c.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), 0)
		return
	case drainNoTurn:
		facts.Attempt = AttemptFacts{NoTurn: true, PendingLeft: att.pendingLeft, CommitRefused: !att.snapshot.Empty()}
		c.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), 0)
		return
	}
	if drainAttemptExempt(att, turnErr) || l.store == nil {
		return
	}
	if turnErr == nil {
		// Reaction chain guard (#1113): account the leg's link BEFORE the
		// debt's post-attempt state is read -- a successful reaction (rows.
		// open == 0 below) must not reset the count.
		c.arb.chainLink(sid, att, att.snapshot)
	}
	pace := func() { c.paceUnreacted(sid, att.hintAt, false, pacePaidUnreacted) }
	if err := l.store.IncrementWakeAttempts(ctx, sid, att.snapshot); err != nil {
		slog.Error("drain attempt: increment wake attempts failed", "session_id", sid, "err", err)
		pace()
		return
	}
	rows, err := c.snapshotAttempts(ctx, sid, att.snapshot)
	if err != nil {
		slog.Error("drain attempt: read wake attempts failed", "session_id", sid, "err", err)
		pace()
		return
	}
	facts.Attempt = AttemptFacts{
		MaxWakeAttempts: rows.max,
		OpenRows:        rows.open,
		AtK:             rows.atK,
		Terminal:        c.drainAttemptTerminal(ctx, att, turnErr, turnCtxDone),
	}
	facts.Snapshot = att.snapshot
	c.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), rows.open)
}

// executeAccountVerdict executes decideAccount's verdict on the real state:
// pacing, resets and the settle-by-failure. openRows is the post-attempt
// count of still-open snapshot rows (the VClose executor keeps the retry
// pace for the rows it did not close).
func (c *coordinator) executeAccountVerdict(ctx context.Context, sid string, att *drainAttempt, turnErr error, v Verdict, openRows int) {
	l := c.asyncJobs
	pacePaid := func() { c.paceUnreacted(sid, att.hintAt, false, pacePaidUnreacted) }
	switch {
	case v.Kind == VDefer && v.Reason == "launch refused":
		// A1: the admission refusal handler paces uncounted and rechecks.
		c.noteDrainRefused(sid, turnErr)
	case v.Kind == VDefer && v.Reason == "pending debt left behind":
		// A2: a no-turn Drain over a pull that keeps failing (free streak).
		c.paceUnreacted(sid, att.hintAt, true, paceFreeNoTurn)
	case v.Kind == VClose:
		// A6: settle exactly the named rows by failure, with the visible
		// marker; rows with fewer attempts keep the retry pace.
		cause := "the assistant responded, but its reaction to this event was not recorded"
		if turnErr != nil {
			cause = redactNetworkURLs(turnErr.Error())
		}
		if err := c.settleDrainDebtTail(ctx, sid, v.Rows, cause, settleTailFor(turnErr)); err != nil {
			pacePaid()
			return
		}
		l.resetGate(sid)
		closed := len(v.Rows.Jobs) + len(v.Rows.Notices)
		if closed < openRows {
			pacePaid()
		}
	case v.Kind == VDefer:
		// A7: a counted, unreacted attempt: the gate paces at R and a newer
		// fact does not open it early (R3B-4).
		pacePaid()
	case v.Reason == "commit refused: gate untouched":
		// A3': the launch verdict refused the commit while visible debt
		// stays; the gate is the launch side's state, not this attempt's
		// evidence -- it is neither opened nor closed. Only the recheck
		// question survives (the old default branch).
		if att.commitNo.recheck {
			c.addToRecheckSet(sid)
		}
	case v.Reason == "no debt: gate reset" || v.Reason == "all rows reacted: gate reset":
		// A3/A5: nothing owed (or every visible row reacted): the gate opens.
		l.resetGate(sid)
	}
}

// drainAttemptTerminal reports whether the attempt's failure closes every row
// it saw at once: a provider classification no retry can fix. A 401 is
// terminal only when the credentials cannot be refreshed: a refresh that
// works (an OAuth token that expired during a long job) makes it a counted,
// paced transient, because the next attempt runs on the new credentials. A 401
// that stays terminal asks the operator to re-authenticate (hyper).
func (c *coordinator) drainAttemptTerminal(ctx context.Context, att *drainAttempt, turnErr error, turnCtxDone bool) bool {
	if !drainFailureTerminal(turnErr, turnCtxDone) {
		return false
	}
	if !c.isUnauthorized(turnErr) {
		return true
	}
	if !att.credentialed && c.refreshAfterUnauthorized(ctx, att.provider) {
		return false
	}
	c.publishReauthenticate(att.provider)
	return true
}

// snapshotAttempts is the per-row picture of a snapshot after an attempt was
// counted on it.
type snapshotAttempts struct {
	// open: rows still debt (still the row the snapshot saw).
	open int
	// max: the highest per-row attempt counter read (0 iff open == 0); the
	// arbiter's A5 input (every visible row reacted).
	max int
	// atK: the rows whose own counter reached drainFailureSettleThreshold.
	atK      session.DebtSnapshot
	atKCount int
}

// snapshotAttempts reads each snapshot row's OWN wake_attempts (one row per
// read: MaxWakeAttempts of a single-row snapshot), so the close can be limited
// to the rows that really used up their attempts.
func (c *coordinator) snapshotAttempts(ctx context.Context, sessionID string, snap session.DebtSnapshot) (snapshotAttempts, error) {
	var out snapshotAttempts
	store := c.asyncJobs.store
	for _, ref := range snap.Jobs {
		n, err := store.MaxWakeAttempts(ctx, sessionID, session.DebtSnapshot{Jobs: []session.DebtJobRef{ref}})
		if err != nil {
			return out, err
		}
		if n == 0 {
			continue // no longer debt
		}
		out.open++
		out.max = max(out.max, n)
		if n >= drainFailureSettleThreshold {
			out.atK.Jobs = append(out.atK.Jobs, ref)
			out.atKCount++
		}
	}
	for _, ref := range snap.Notices {
		n, err := store.MaxWakeAttempts(ctx, sessionID, session.DebtSnapshot{Notices: []session.DebtNoticeRef{ref}})
		if err != nil {
			return out, err
		}
		if n == 0 {
			continue
		}
		out.open++
		out.max = max(out.max, n)
		if n >= drainFailureSettleThreshold {
			out.atK.Notices = append(out.atK.Notices, ref)
			out.atKCount++
		}
	}
	return out, nil
}

// noteDrainRefused is the one handler of an admission refusal (lock busy or
// unwritable, model persist failure, provider not configured, peak hours,
// shutdown, an unreadable pre-turn DB read). A refusal is neither counted nor
// settled: the debt stays and the launch is retried -- at once (every 0.5s)
// by a `rush run` loop that drives the session itself, at the next re-check
// tick otherwise. A newer fact opens the gate early.
func (c *coordinator) noteDrainRefused(sessionID string, cause error) {
	l := c.asyncJobs
	if l == nil || sessionID == "" {
		return
	}
	external := l.isExternalDriver(sessionID)
	wait := drainRetryAfterFailure()
	if external {
		wait = drainRefusalPauseLoop()
	}
	l.arbPace(sessionID, l.hintSeqOf(sessionID), wait, true, paceUncounted)
	if !external {
		c.addToRecheckSet(sessionID)
	}
	slog.Debug("drain launch refused; the debt stays", "session_id", sessionID, "err", cause)
}

// noteTurnFailed paces the gate after an ordinary (non-Drain) turn ended in a
// provider failure: no paid reaction turn right behind a failed user turn.
// Nothing is counted; a newer fact does not open the gate early.
func (c *coordinator) noteTurnFailed(sessionID string) {
	if c.asyncJobs == nil || sessionID == "" {
		return
	}
	c.asyncJobs.arbPace(sessionID, 0, drainRetryAfterFailure(), false, paceUncounted)
}

// settleDrainDebt closes the snapshot's debt by failure and writes the
// visible wake_failed marker in one transaction (marker only if a row was
// really settled). The marker names the snapshot's own tool_call_ids.
func (c *coordinator) settleDrainDebt(ctx context.Context, sessionID string, snap session.DebtSnapshot, cause string) error {
	return c.settleDrainDebtTail(ctx, sessionID, snap, cause, settleTailNextTurn)
}

// settleDrainDebtTail is settleDrainDebt with the marker's closing sentence
// chosen by the caller (settleTailFor).
func (c *coordinator) settleDrainDebtTail(ctx context.Context, sessionID string, snap session.DebtSnapshot, cause, tail string) error {
	if c.asyncJobs == nil || c.asyncJobs.store == nil || snap.Empty() {
		return nil
	}
	ids := make([]string, 0, len(snap.Jobs))
	for _, ref := range snap.Jobs {
		id, _, _ := strings.Cut(ref.ToolCallID, "#reused#")
		ids = append(ids, id)
	}
	var text string
	if len(ids) > 0 {
		text = fmt.Sprintf(
			"Не удалось продолжить работу после события %s: %s. %s",
			strings.Join(ids, ", "), cause, tail)
	} else {
		text = fmt.Sprintf(
			"Не удалось продолжить работу после ошибки провайдера: %s. %s",
			cause, tail)
	}
	settled, err := c.asyncJobs.store.SettleReactedFailedWithMarker(ctx, sessionID, snap, text)
	if err != nil {
		slog.Error("drain attempt: settle by failure failed", "session_id", sessionID, "err", err)
		return err
	}
	if settled == 0 {
		slog.Debug("drain attempt: snapshot already resolved, nothing settled", "session_id", sessionID)
	}
	return nil
}

// noteRefusal reports a pre-loop refusal of any call kind to the gate: the
// release that follows must not immediately relaunch a Drain.
func (a *sessionAgent) noteRefusal(sessionID string, cause error) {
	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
		return
	}
	a.asyncJobs.coord.noteDrainRefused(sessionID, cause)
}

// afterTurn is runOwned's per-leg epilogue, run right after runTurn returns
// and before the mailbox can be released: it accounts a Drain leg runTurn did
// not already close, suspends automatic turns while a question the agent
// asked awaits its answer, paces the gate after an ordinary turn that ended
// in a provider failure, and pushes the supervision deadline unless the leg
// was a Drain that never reached the provider. It returns the error runOwned
// should report (a Drain that never reached the provider says so).
func (a *sessionAgent) afterTurn(call SessionAgentCall, att *drainAttempt, err error, turnCtxDone bool) error {
	if att != nil && turnCtxDone {
		att.turnCtxDone.Store(true)
	}
	a.closeDrainAttempt(att, err)
	if a.asyncJobs != nil {
		var awaiting *AwaitingAnswerError
		coord := a.asyncJobs.coord
		switch {
		case coord != nil && errors.As(err, &awaiting):
			coord.suspendAutoResume(call.SessionID)
		case coord != nil && att == nil && err != nil && providerTurnFailed(err, turnCtxDone):
			coord.noteTurnFailed(call.SessionID)
		}
		if att == nil || att.outcome == drainAttempted {
			a.asyncJobs.pushDeadlineOnTurnEnd(call.SessionID)
		}
	}
	if att != nil && att.outcome == drainNotAttempted {
		return notAttempted(call, err)
	}
	return err
}

// providerTurnFailed reports whether err is a real provider failure of an
// ordinary turn (not an operator stop, not the peak-hours window). Same rule
// as the Drain accounting: a transport timeout of a live turn is a failure.
func providerTurnFailed(err error, turnCtxDone bool) bool {
	if operatorStop(err, turnCtxDone) || errors.Is(err, errProviderPeakHours) {
		return false
	}
	return isProviderClassifiable(err)
}

// paceUnreacted shuts the gate after an unreacted outcome and queues the
// session for a re-check tick. The moment a streak turns the gate dormant it
// says so once, naming what reopens it: the debt stays visible (`sessions
// why`). A pull that keeps failing (free) is reopened by a newer event or a
// human message; paid attempts whose close kept failing only by a human
// message or a restart.
func (c *coordinator) paceUnreacted(sessionID string, hintAt uint64, hintOpens bool, kind drainPace) {
	if c.asyncJobs.arbPace(sessionID, hintAt, drainRetryAfterFailure(), hintOpens, kind) {
		if kind == paceFreeNoTurn {
			slog.Warn("drain launches for this session are paused: the notice pull keeps failing; a new event or a human message resumes them",
				"session_id", sessionID)
		} else {
			slog.Warn("drain launches for this session are paused: repeated attempts could not be closed or counted; only a human message (or a restart) resumes them",
				"session_id", sessionID)
		}
	}
	c.addToRecheckSet(sessionID)
}

// drainRefused reports a pre-agent refusal (provider not configured, peak
// hours, model resolution, shutdown) of a Drain call built through the
// prompt-string entry points: the launch gate is paced and the error says the
// Drain never reached the provider. Any other call keeps its error as is.
func (c *coordinator) drainRefused(ctx context.Context, sessionID string, err error) error {
	if err == nil || !isDrainCallFrom(ctx) {
		return err
	}
	c.noteDrainRefused(sessionID, err)
	return &DrainNotAttemptedError{Err: err}
}
