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
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/session"
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
	driverMarkerStore() batchflow.BatchDriverClaimer
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

func (h coordinatorDrainHost) driverMarkerStore() batchflow.BatchDriverClaimer {
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
	return modelkit.IsUnauthorized(err)
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

func operatorStop(err error, turnCtxDone bool) bool {
	return drainctl.OperatorStop(err, turnCtxDone)
}

// drainAttemptExempt and the account/verdict/terminal/pacing bodies moved to
// drainctl (AccountAttempt, ExecuteAccountVerdict, AttemptTerminal,
// NoteDrainRefused, PaceUnreacted); what stays here are the record
// projection and the governor/port adapters.

// drainAccountRecord projects the private drainAttempt record onto the leaf
// accounting's AttemptRecord: atomics are read through the record's own Load
// methods at use time, and the batchDebtCallIDs assignment goes back to the
// same field (the accounting sets it BEFORE the reaction chain guard).
func drainAccountRecord(att *drainAttempt) drainctl.AttemptRecord {
	return drainctl.AttemptRecord{
		SessionID:       att.sessionID,
		HintAt:          att.hintAt,
		Outcome:         att.outcome,
		PendingLeft:     att.pendingLeft,
		CommitNoRecheck: att.commitNo.recheck,
		StreamErr:       att.streamErr,
		Stalled:         att.stalled.Load,
		CapAbort:        att.capAbort.Load,
		TurnCtxDone:     att.turnCtxDone.Load,
		Provider:        att.provider,
		Credentialed:    att.credentialed,
		BatchSnapshot:   att.batchSnapshot,
		BatchCaptured:   att.batchCaptured,
		SetBatchDebtCallIDs: func(ids []string) {
			att.batchDebtCallIDs = ids
		},
	}
}

// drainAccountGovernor adapts the drainHost onto the leaf accounting's
// AttemptGovernor: the arbiter state's writers (R-ARB-2) and the 401
// predicates, with the attempt record carried for the chain guard.
type drainAccountGovernor struct {
	host drainHost
	att  *drainAttempt
}

func (g drainAccountGovernor) AccountFacts() drainctl.AttemptPort {
	return drainctlAttemptPort{facts: g.host.batchCutoverFacts()}
}

func (g drainAccountGovernor) Now() time.Time { return g.host.arbiter().now() }

func (g drainAccountGovernor) Pace(sid string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainctl.Pace) (becameDormant bool) {
	return g.host.arbiter().pace(sid, hintAt, wait, hintOpens, kind)
}

func (g drainAccountGovernor) ChainLink(sid string, snap session.BatchDebtSnapshot) {
	g.host.arbiter().chainLink(sid, g.att, snap)
}

func (g drainAccountGovernor) ResetGate(sid string)       { g.host.arbiter().resetGate(sid) }
func (g drainAccountGovernor) AddToRecheckSet(sid string) { g.host.addToRecheckSet(sid) }

func (g drainAccountGovernor) IsExternalDriver(sid string) bool {
	return g.host.driverHints().isExternalDriver(sid)
}

func (g drainAccountGovernor) HintSeqOf(sid string) uint64 {
	return g.host.driverHints().hintSeqOf(sid)
}

func (g drainAccountGovernor) IsUnauthorized(err error) bool { return g.host.isUnauthorized(err) }

func (g drainAccountGovernor) RefreshAfterUnauthorized(ctx context.Context, providerID string) bool {
	return g.host.refreshAfterUnauthorized(ctx, providerID)
}

func (g drainAccountGovernor) PublishReauthenticate(providerID string) {
	g.host.publishReauthenticate(providerID)
}

func (g drainAccountGovernor) RedactURLs(s string) string { return modelkit.RedactNetworkURLs(s) }

// drainctlAttemptPort projects the root BatchCutoverFacts onto the leaf
// accounting's narrow AttemptPort (only AttemptRows' shape differs).
type drainctlAttemptPort struct{ facts BatchCutoverFacts }

func (p drainctlAttemptPort) DebtToolCallIDs(ctx context.Context, snap session.BatchDebtSnapshot) []string {
	return p.facts.DebtToolCallIDs(ctx, snap)
}

func (p drainctlAttemptPort) IncrementWakeAttempts(ctx context.Context, owner string, snap session.BatchDebtSnapshot) error {
	return p.facts.IncrementWakeAttempts(ctx, owner, snap)
}

func (p drainctlAttemptPort) AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (drainctl.AttemptRows, error) {
	r, err := p.facts.AttemptRows(ctx, owner, snap, atK)
	return drainctl.AttemptRows{Open: r.Open, Max: r.Max, AtK: r.AtK, UnmappedAtK: r.UnmappedAtK}, err
}

func (p drainctlAttemptPort) SettleReactedFailedWithMarker(ctx context.Context, owner string, snap session.BatchDebtSnapshot, text string) (int64, error) {
	return p.facts.SettleReactedFailedWithMarker(ctx, owner, snap, text)
}

// drainFailureTerminal is the root wrapper of drainctl.DrainFailureTerminal,
// passed to the leaf accounting as a use-time callback so the peak-hours
// sentinel and the provider-error classification are read only when the
// failure classification actually runs, not before the DB callbacks.
func drainFailureTerminal(err error, turnCtxDone bool) bool {
	return drainctl.DrainFailureTerminal(err, turnCtxDone, errProviderPeakHours, func(e error) bool {
		return classifyProviderError(e) == classTerminal
	})
}

// accountDrainAttempt is THE accounting function of a Drain leg; the
// implementation lives in drainctl (AccountAttempt) behind the record
// projection and the governor above.
func (w *drainComponent) accountDrainAttempt(ctx context.Context, att *drainAttempt, turnErr error) {
	if w.host.coordinatorIsNil() || att == nil {
		return
	}
	drainctl.AccountAttempt(ctx, drainAccountGovernor{host: w.host, att: att}, drainAccountRecord(att), turnErr, drainFailureTerminal)
}

// noteDrainRefused is the one handler of an admission refusal; the
// implementation lives in drainctl (NoteDrainRefused).
func (w *drainComponent) noteDrainRefused(sessionID string, cause error) {
	if w.host.coordinatorIsNil() || sessionID == "" {
		return
	}
	drainctl.NoteDrainRefused(drainAccountGovernor{host: w.host}, sessionID, cause)
}

// noteTurnFailed paces the gate after an ordinary (non-Drain) turn ended in a
// provider failure: no paid reaction turn right behind a failed user turn.
// Nothing is counted; a newer fact does not open the gate early.
func (w *drainComponent) noteTurnFailed(sessionID string) {
	if w.host.coordinatorIsNil() || sessionID == "" {
		return
	}
	w.host.arbiter().pace(sessionID, 0, drainRetryAfterFailure(), false, batchlife.PaceUncounted)
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
