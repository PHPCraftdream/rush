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

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/session"
	runq "github.com/PHPCraftdream/rush/internal/session/run/sessionrun"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// drainHost is the narrow coordinator surface the drain/reaction/arbiter
// component needs (the wakeComponent pattern: the component never holds the
// raw *coordinator, only this host).
type drainHost interface {
	coordinatorIsNil() bool
	arbiter() *arbiter
	driverHints() *batchDriverHints
	sessionNoticeStore() *session.SessionNoticeStore
	driverMarkerStore() BatchDriverClaimer
	sessionsService() coresession.Service
	persistentMode() bool
	backgroundPendingCompletions(sessionID string) bool
	openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error)
	startRecheckTicker()
	batchCutoverFacts() BatchCutoverFacts
	batchCutoverReactionStore() *session.BatchStore
	readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts) (TurnFacts, error)
	recoverBatchScopeDeadCandidates(ctx context.Context, sessionID string)
	clearSleepAll(sessionID string)
	addToRecheckSet(sessionID string)
	isUnauthorized(err error) bool
	refreshAfterUnauthorized(ctx context.Context, providerID string) bool
	publishReauthenticate(providerID string)
}

type (
	drainComponent       struct{ host drainHost }
	coordinatorDrainHost struct{ coordinator *coordinator }
)

func newDrainComponent(c *coordinator) *drainComponent {
	return &drainComponent{host: coordinatorDrainHost{coordinator: c}}
}

func (h coordinatorDrainHost) coordinatorIsNil() bool { return h.coordinator == nil }
func (h coordinatorDrainHost) arbiter() *arbiter      { return &h.coordinator.arb }
func (h coordinatorDrainHost) driverHints() *batchDriverHints {
	return h.coordinator.driverHints
}

func (h coordinatorDrainHost) sessionNoticeStore() *session.SessionNoticeStore {
	return h.coordinator.sessionNoticeStore()
}

func (h coordinatorDrainHost) driverMarkerStore() BatchDriverClaimer {
	return h.coordinator.driverMarkerStore()
}

func (h coordinatorDrainHost) sessionsService() coresession.Service { return h.coordinator.sessions }

func (h coordinatorDrainHost) persistentMode() bool { return h.coordinator.persistentMode.Load() }

func (h coordinatorDrainHost) backgroundPendingCompletions(sessionID string) bool {
	return h.coordinator.background != nil && h.coordinator.background.PendingCompletionsOwned(sessionID) > 0
}

func (h coordinatorDrainHost) openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	return h.coordinator.openOnceWakeSchedules(ctx, sessionID)
}
func (h coordinatorDrainHost) startRecheckTicker() { h.coordinator.StartRecheckTicker() }
func (h coordinatorDrainHost) batchCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(h.coordinator)
}

func (h coordinatorDrainHost) batchCutoverReactionStore() *session.BatchStore {
	return batchCutoverReactionStore(h.coordinator)
}

func (h coordinatorDrainHost) readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts) (TurnFacts, error) {
	return h.coordinator.readTurnFactsBatch(ctx, facts, sessionID, f)
}

func (h coordinatorDrainHost) recoverBatchScopeDeadCandidates(ctx context.Context, sessionID string) {
	recoverBatchScopeDeadCandidates(ctx, h.coordinator, sessionID)
}

func (h coordinatorDrainHost) clearSleepAll(sessionID string) { h.coordinator.clearSleepAll(sessionID) }

func (h coordinatorDrainHost) addToRecheckSet(sessionID string) {
	h.coordinator.addToRecheckSet(sessionID)
}

func (h coordinatorDrainHost) isUnauthorized(err error) bool {
	return h.coordinator.isUnauthorized(err)
}

func (h coordinatorDrainHost) refreshAfterUnauthorized(ctx context.Context, providerID string) bool {
	return h.coordinator.refreshAfterUnauthorized(ctx, providerID)
}

func (h coordinatorDrainHost) publishReauthenticate(providerID string) {
	h.coordinator.publishReauthenticate(providerID)
}

type drainOutcome = drainctl.DrainOutcome

const (
	drainNotAttempted = drainctl.DrainNotAttempted
	drainNoTurn       = drainctl.DrainNoTurn
	drainAttempted    = drainctl.DrainAttempted
)

// drainFailureSettleThreshold is K: a row whose OWN counter reaches this many
// counted, unreacted attempts is closed by failure (other rows keep theirs).
const drainFailureSettleThreshold = drainctl.FailureSettleThreshold

// drainDormantStreak is the length of either dormancy streak (the arbiter gate):
// consecutive no-turn Drains over a failing pull (a newer fact reopens the
// gate) or consecutive paid attempts whose close kept failing (only a human
// message or a restart reopens it).
const drainDormantStreak = drainctl.DormantStreak

// The pacing atomics live in runtime/exec/drainctl/drain_attempt.go
// (drainRetryAfterNS etc.); these root adapters keep the historical names for
// the lazy reads and the tests below.
var (
	drainRetryAfterNS    = drainctlPacingVar{drainctl.LoadDrainRetryAfterNS, drainctl.SwapDrainRetryAfterNS, drainctl.StoreDrainRetryAfterNS}
	drainRefusalPauseNS  = drainctlPacingVar{drainctl.LoadDrainRefusalPauseNS, drainctl.SwapDrainRefusalPauseNS, drainctl.StoreDrainRefusalPauseNS}
	drainAccountBudgetNS = drainctlPacingVar{drainctl.LoadDrainAccountBudgetNS, drainctl.SwapDrainAccountBudgetNS, drainctl.StoreDrainAccountBudgetNS}
	drainRefreshBudgetNS = drainctlPacingVar{drainctl.LoadDrainRefreshBudgetNS, drainctl.SwapDrainRefreshBudgetNS, drainctl.StoreDrainRefreshBudgetNS}
)

func drainRetryAfterFailure() time.Duration { return drainctl.RetryAfter() }
func drainRefusalPauseLoop() time.Duration  { return drainctl.RefusalPause() }
func drainAccountBudget() time.Duration     { return drainctl.AccountBudget() }
func drainRefreshBudget() time.Duration     { return drainctl.RefreshBudget() }

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
	// snapshot is retained struct shape only: batch accounting reads
	// batchSnapshot exclusively (att.snapshot stays zero).
	snapshot session.BatchDebtSnapshot
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
	// chainBatchRoots holds the root tool_call_ids of the batch launches
	// THIS leg started whose classification was a pure wait (the started
	// tool response's own identity: the launched root's tool_call_id IS the
	// call id). Each root's selected completion becomes a node debt row
	// keyed by that root id; read by the reaction chain guard's batch
	// accounting.
	chainBatchRoots []string
	// batchSnapshot is the batch twin of snapshot: the visible batch debt
	// captured right after the leg's own turn-start pull -- the ONLY set
	// this leg's accounting may increment on or settle. batchCaptured says
	// the capture succeeded; a failed capture is NEVER re-read late at
	// accounting time (a late capture would widen the acted-on set with
	// rows that arrived after the pull).
	batchSnapshot session.BatchDebtSnapshot
	batchCaptured bool
	// batchDebtCallIDs is batchSnapshot's node rows resolved to their
	// ROOT tool_call_ids (the chain identity vocabulary), computed by the
	// accounting right before the chain guard reads it.
	batchDebtCallIDs []string
	closed           bool
}

// newDrainAttempt starts the accounting record of a Drain call's leg; nil for
// every other call kind. Called before the leg's turn-start pull.
func (a *sessionAgent) newDrainAttempt(call SessionAgentCall) *drainAttempt {
	if !call.IsDrain {
		return nil
	}
	att := &drainAttempt{sessionID: call.SessionID}
	if a.coord != nil {
		att.hintAt = a.coord.driverHints.hintSeqOf(call.SessionID)
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
	if a.coord == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
	defer cancel()
	a.coord.accountDrainAttempt(ctx, att, turnErr)
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

func operatorStop(err error, turnCtxDone bool) bool {
	return drainctl.OperatorStop(err, turnCtxDone)
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

func drainFailureTerminal(err error, turnCtxDone bool) bool {
	return drainctl.DrainFailureTerminal(err, turnCtxDone, errProviderPeakHours, func(e error) bool {
		return classifyProviderError(e) == classTerminal
	})
}

// accountDrainAttempt is THE accounting function of a Drain leg. The
// DECISION is decideAccount (the accounting rows A1-A7 of the arbiter
// table); this function gathers the attempt's facts, asks the arbiter, and
// executes the verdict -- the gate/streak/chain writes themselves are the
// arbiter state's writers (R-ARB-2). The DB failure paths (a failed count, a
// failed snapshot read, a failed settle) keep today's behavior: pace the
// gate like an unreacted paid attempt and stay in the recheck set.
func (w *drainComponent) accountDrainAttempt(ctx context.Context, att *drainAttempt, turnErr error) {
	if w.host.coordinatorIsNil() || att == nil || att.sessionID == "" {
		return
	}
	sid := att.sessionID
	turnErr = att.failure(turnErr)
	turnCtxDone := att.turnCtxDone.Load()
	facts := TurnFacts{Now: w.host.arbiter().now(), Site: siteAccount}
	facts.Session.ExternallyDrivenSelf = w.host.driverHints().isExternalDriver(sid)
	// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1
	// "drain/reaction"): the leg's error is the ONLY place the run-queue/
	// runOwned ownership refusal is visible from here -- the DB half of the
	// facts holds no workspace column. Stamping it here is what makes
	// decideAccount defer that leg without pacing or settling anything.
	facts.Session.ForeignWorkspace = errors.Is(turnErr, runq.ErrForeignWorkspace)
	switch att.outcome {
	case drainNotAttempted:
		if turnErr == nil || operatorStop(turnErr, turnCtxDone) {
			return // a cancelled preamble: nothing was refused, nothing was tried
		}
		facts.Attempt = AttemptFacts{NotAttempted: true, Refused: true}
		w.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), 0)
		return
	case drainNoTurn:
		// R12-E fix: with the batch runtime on, the pull-time capture fills
		// att.batchSnapshot and leaves att.snapshot zero (agent_turn.go), so
		// visible batch debt is exactly a nonempty batch snapshot. Without
		// this term a flag-ON policy refusal reported CommitRefused=false
		// and fell through to "no debt: gate reset", clearing a gate the
		// attempt produced no evidence about. A failed capture cannot reach
		// the no-turn path: the ON capture either fails the turn (error
		// return) or runs uncounted committed (the !commit branch is
		// skipped for DrainTurnCommitted), so att.batchCaptured true here
		// means the capture succeeded.
		batchRefused := att.batchCaptured && !att.batchSnapshot.Empty()
		facts.Attempt = AttemptFacts{NoTurn: true, PendingLeft: att.pendingLeft, CommitRefused: batchRefused}
		w.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), 0)
		return
	}
	if drainAttemptExempt(att, turnErr) {
		return
	}
	pace := func() { w.paceUnreacted(sid, att.hintAt, false, pacePaidUnreacted) }
	if turnErr == nil {
		// Reaction chain guard (#1113): account the leg's link BEFORE the
		// debt's post-attempt state is read -- a successful reaction (rows.
		// open == 0 below) must not reset the count. On the batch path a leg
		// whose pull-time capture failed is as undocumented as a failed
		// legacy snapshot read: skip the link too (same conservatism as the
		// accounting below, which will not count it). The batch guard
		// compares ROOT tool_call_ids, so the acted-on rows are resolved
		// into the identity vocabulary first.
		bfacts := w.host.batchCutoverFacts()
		if !att.batchCaptured {
			slog.Warn("drain attempt: pull-time batch debt capture missing; link unaccounted",
				"session_id", sid)
			pace()
			return
		}
		att.batchDebtCallIDs = bfacts.DebtToolCallIDs(ctx, att.batchSnapshot)
		w.host.arbiter().chainLink(sid, att, att.batchSnapshot)
	}
	// Count and reread through the ACTED-ON snapshot captured at
	// the turn-start pull (att.batchSnapshot). A leg without that capture is
	// not counted and settles nothing -- a late recapture would widen the
	// acted-on set with rows that arrived after the pull.
	{
		bfacts := w.host.batchCutoverFacts()
		if !att.batchCaptured {
			slog.Warn("drain attempt: no pull-time batch debt snapshot; attempt uncounted",
				"session_id", sid)
			pace()
			return
		}
		snap := att.batchSnapshot
		if err := bfacts.IncrementWakeAttempts(ctx, sid, snap); err != nil {
			slog.Error("drain attempt: increment wake attempts failed", "session_id", sid, "err", err)
			pace()
			return
		}
		batchRows, err := bfacts.AttemptRows(ctx, sid, snap, drainFailureSettleThreshold)
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
			Terminal:        w.drainAttemptTerminal(ctx, att, turnErr, turnCtxDone),
		}
		facts.BatchSnapshot = snap
		w.executeAccountVerdict(ctx, sid, att, turnErr, decideAccount(facts), batchRows.Open)
	}
}

// executeAccountVerdict executes decideAccount's verdict on the real state:
// pacing, resets and the settle-by-failure. openRows is the post-attempt
// count of still-open snapshot rows (the VClose executor keeps the retry
// pace for the rows it did not close).
func (w *drainComponent) executeAccountVerdict(ctx context.Context, sid string, att *drainAttempt, turnErr error, v Verdict, openRows int) {
	pacePaid := func() { w.paceUnreacted(sid, att.hintAt, false, pacePaidUnreacted) }
	switch {
	case v.Kind == VDefer && v.Reason == "launch refused":
		// A1: the admission refusal handler paces uncounted and rechecks.
		w.noteDrainRefused(sid, turnErr)
	case v.Kind == VDefer && v.Reason == "pending debt left behind":
		// A2: a no-turn Drain over a pull that keeps failing (free streak).
		w.paceUnreacted(sid, att.hintAt, true, paceFreeNoTurn)
	case v.Kind == VClose:
		// A6: settle exactly the named rows by failure, with the visible
		// marker; rows with fewer attempts keep the retry pace.
		cause := "the assistant responded, but its reaction to this event was not recorded"
		if turnErr != nil {
			cause = redactNetworkURLs(turnErr.Error())
		}
		if bfacts := w.host.batchCutoverFacts(); !v.BatchRows.Empty() {
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
					strings.Join(trimmed, ", "), cause, drainctl.SettleTailFor(turnErr))
			} else {
				text = fmt.Sprintf(
					"Не удалось продолжить работу после ошибки провайдера: %s. %s",
					cause, drainctl.SettleTailFor(turnErr))
			}
			if _, err := bfacts.SettleReactedFailedWithMarker(ctx, sid, v.BatchRows, text); err != nil {
				pacePaid()
				return
			}
			w.host.arbiter().resetGate(sid)
			closed := len(v.BatchRows.Nodes) + len(v.BatchRows.Notices)
			if closed < openRows {
				pacePaid()
			}
			return
		}
	case v.Kind == VDefer && v.Reason == foreignWorkspaceReason:
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
		if att.commitNo.recheck {
			w.host.addToRecheckSet(sid)
		}
	case v.Reason == "no debt: gate reset" || v.Reason == "all rows reacted: gate reset":
		// A3/A5: nothing owed (or every visible row reacted): the gate opens.
		w.host.arbiter().resetGate(sid)
	}
}

// drainAttemptTerminal reports whether the attempt's failure closes every row
// it saw at once: a provider classification no retry can fix. A 401 is
// terminal only when the credentials cannot be refreshed: a refresh that
// works (an OAuth token that expired during a long job) makes it a counted,
// paced transient, because the next attempt runs on the new credentials. A 401
// that stays terminal asks the operator to re-authenticate (hyper).
func (w *drainComponent) drainAttemptTerminal(ctx context.Context, att *drainAttempt, turnErr error, turnCtxDone bool) bool {
	if !drainFailureTerminal(turnErr, turnCtxDone) {
		return false
	}
	if !w.host.isUnauthorized(turnErr) {
		return true
	}
	if !att.credentialed && w.host.refreshAfterUnauthorized(ctx, att.provider) {
		return false
	}
	w.host.publishReauthenticate(att.provider)
	return true
}

// noteDrainRefused is the one handler of an admission refusal (lock busy or
// unwritable, model persist failure, provider not configured, peak hours,
// shutdown, an unreadable pre-turn DB read). A refusal is neither counted nor
// settled: the debt stays and the launch is retried -- at once (every 0.5s)
// by a `rush run` loop that drives the session itself, at the next re-check
// tick otherwise. A newer fact opens the gate early.
func (w *drainComponent) noteDrainRefused(sessionID string, cause error) {
	if w.host.coordinatorIsNil() || sessionID == "" {
		return
	}
	external := w.host.driverHints().isExternalDriver(sessionID)
	wait := drainRetryAfterFailure()
	if external {
		wait = drainRefusalPauseLoop()
	}
	w.host.arbiter().pace(sessionID, w.host.driverHints().hintSeqOf(sessionID), wait, true, paceUncounted)
	if !external {
		w.host.addToRecheckSet(sessionID)
	}
	slog.Debug("drain launch refused; the debt stays", "session_id", sessionID, "err", cause)
}

// noteTurnFailed paces the gate after an ordinary (non-Drain) turn ended in a
// provider failure: no paid reaction turn right behind a failed user turn.
// Nothing is counted; a newer fact does not open the gate early.
func (w *drainComponent) noteTurnFailed(sessionID string) {
	if w.host.coordinatorIsNil() || sessionID == "" {
		return
	}
	w.host.arbiter().pace(sessionID, 0, drainRetryAfterFailure(), false, paceUncounted)
}

// noteRefusal reports a pre-loop refusal of any call kind to the gate: the
// release that follows must not immediately relaunch a Drain.
func (a *sessionAgent) noteRefusal(sessionID string, cause error) {
	if a.coord == nil {
		return
	}
	a.coord.noteDrainRefused(sessionID, cause)
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
	if a.coord != nil {
		var awaiting *AwaitingAnswerError
		coord := a.coord
		switch {
		case errors.As(err, &awaiting):
			coord.suspendAutoResume(call.SessionID)
		case att == nil && err != nil && providerTurnFailed(err, turnCtxDone):
			coord.noteTurnFailed(call.SessionID)
		}
		if att == nil || att.outcome == drainAttempted {
			coord.pushDeadlineOnTurnEnd(call.SessionID)
		}
	}
	if att != nil && att.outcome == drainNotAttempted {
		return notAttempted(call, err)
	}
	return err
}

func providerTurnFailed(err error, turnCtxDone bool) bool {
	return drainctl.ProviderTurnFailed(err, turnCtxDone, errProviderPeakHours, isProviderClassifiable)
}

// paceUnreacted shuts the gate after an unreacted outcome and queues the
// session for a re-check tick. The moment a streak turns the gate dormant it
// says so once, naming what reopens it: the debt stays visible (`sessions
// why`). A pull that keeps failing (free) is reopened by a newer event or a
// human message; paid attempts whose close kept failing only by a human
// message or a restart.
func (w *drainComponent) paceUnreacted(sessionID string, hintAt uint64, hintOpens bool, kind drainPace) {
	if w.host.arbiter().pace(sessionID, hintAt, drainRetryAfterFailure(), hintOpens, kind) {
		if kind == paceFreeNoTurn {
			slog.Warn("drain launches for this session are paused: the notice pull keeps failing; a new event or a human message resumes them",
				"session_id", sessionID)
		} else {
			slog.Warn("drain launches for this session are paused: repeated attempts could not be closed or counted; only a human message (or a restart) resumes them",
				"session_id", sessionID)
		}
	}
	w.host.addToRecheckSet(sessionID)
}

// drainRefused reports a pre-agent refusal (provider not configured, peak
// hours, model resolution, shutdown) of a Drain call built through the
// prompt-string entry points: the launch gate is paced and the error says the
// Drain never reached the provider. Any other call keeps its error as is.
func (w *drainComponent) drainRefused(ctx context.Context, sessionID string, err error) error {
	if err == nil || !isDrainCallFrom(ctx) {
		return err
	}
	w.noteDrainRefused(sessionID, err)
	return &DrainNotAttemptedError{Err: err}
}

func (a *sessionAgent) decideDrainTurn(ctx context.Context, sessionID string, hasVisibleDebt bool) (bool, drainVerdict) {
	if !hasVisibleDebt {
		return false, drainVerdict{kind: drainDeferred, reason: "no visible debt"}
	}
	if a.coord == nil {
		return true, drainVerdict{kind: drainAllow}
	}
	// The commit compares the bg-shell cap like a re-check (siteCommit: no
	// slot): a QUEUED Drain decides on debt its launch decision never saw (a
	// completion over the cap that landed after the Drain that owned the last
	// slot pulled), and it must not get a sixth automatic turn. A Drain's own
	// slot row is not an over-cap row, so it is never refused for the slot it
	// holds.
	v, _, err := a.coord.arbiterVerdict(ctx, sessionID, siteCommit)
	if err != nil {
		return false, unreadablePolicyInput("launch decision input", sessionID, err)
	}
	dv := drainVerdictOf(v)
	if dv.kind == drainAllow {
		// An allowed drain launch breaks await_tasks' sleep and cancels its
		// max_wait schedule (#1270). The wakeSession funnel (drainPolicy)
		// never sees an externally-driven session's drains — its loop
		// launches those itself — so this is the commit-side half of the
		// "every allowed launch clears" contract.
		a.coord.clearSleepAll(sessionID)
	}
	return dv.kind == drainAllow, dv
}

func SetDrainPacingForTest(retryAfter, refusalPause, recheckInterval time.Duration) (restore func()) {
	return drainctl.DrainPacingForTest(retryAfter, refusalPause, recheckInterval)
}
