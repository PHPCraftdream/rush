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
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Drain attempt accounting (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md): ONE accounting point for a Drain leg, run by the turn
// loop that ran it -- exactly once, after the leg ends and before the mailbox
// is released -- from the DB's own verdict (are the rows this attempt could
// see still unreacted?). Launchers never account: a queued or merged Drain has
// no launcher, and the release hook fires inside Run before a launcher
// regains control. Launch decisions read the per-session gate this file
// writes (work_ledger_reaction.go, coordinator_drain_policy.go).
//
// The implementation moved to drain_component.go (the drainComponent host
// pattern, like wake_component.go); this file keeps the thin coordinator
// delegates.
func (c *coordinator) accountDrainAttempt(ctx context.Context, att *drainctl.DrainAttempt, turnErr error) {
	newDrainComponent(c).accountDrainAttempt(ctx, att, turnErr)
}

func (c *coordinator) AccountDrainAttempt(ctx context.Context, att agentcall.CoordinatorDrainAttempt, turnErr error) {
	attempt, _ := att.(*drainctl.DrainAttempt)
	c.accountDrainAttempt(ctx, attempt, turnErr)
}

func (c *coordinator) NoteDrainRefused(sessionID string, cause error) {
	c.noteDrainRefused(sessionID, cause)
}
func (c *coordinator) NoteTurnFailed(sessionID string) { c.noteTurnFailed(sessionID) }
func (c *coordinator) noteDrainRefused(sessionID string, cause error) {
	newDrainComponent(c).noteDrainRefused(sessionID, cause)
}

func (c *coordinator) noteTurnFailed(sessionID string) {
	newDrainComponent(c).noteTurnFailed(sessionID)
}

func (c *coordinator) drainRefused(ctx context.Context, sessionID string, err error) error {
	return newDrainComponent(c).drainRefused(ctx, sessionID, err)
}

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
	return newBatchCutoverGlueComponent(h.coordinator).driverMarkerStore()
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

type drainAttempt = drainctl.DrainAttempt

// newDrainAttempt starts the accounting record of a Drain call's leg; nil for
// every other call kind. Called before the leg's turn-start pull.
func (a *sessionAgent) newDrainAttempt(call SessionAgentCall) *drainAttempt {
	if !call.IsDrain {
		return nil
	}
	att := &drainAttempt{SessionID: call.SessionID}
	if a.coord != nil {
		att.HintAt = a.coord.DriverHints().HintSeqOf(call.SessionID)
	}
	return att
}

// closeDrainAttempt accounts att exactly once (idempotent; nil-safe). It runs
// on a context detached from the turn: a cancelled turn's accounting is
// decided by drainAttemptExempt, never by the cancellation itself.
func (a *sessionAgent) closeDrainAttempt(att *drainAttempt, turnErr error) {
	if att == nil || att.Closed {
		return
	}
	att.Closed = true
	if a.coord == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainAccountBudget())
	defer cancel()
	a.coord.AccountDrainAttempt(ctx, att, turnErr)
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
		SessionID:       att.SessionID,
		HintAt:          att.HintAt,
		Outcome:         att.Outcome,
		PendingLeft:     att.PendingLeft,
		CommitNoRecheck: att.CommitNo.Recheck,
		StreamErr:       att.StreamErr,
		Stalled:         att.Stalled.Load,
		CapAbort:        att.CapAbort.Load,
		TurnCtxDone:     att.TurnCtxDone.Load,
		Provider:        att.Provider,
		Credentialed:    att.Credentialed,
		BatchSnapshot:   att.BatchSnapshot,
		BatchCaptured:   att.BatchCaptured,
		SetBatchDebtCallIDs: func(ids []string) {
			att.BatchDebtCallIDs = ids
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
	a.coord.NoteDrainRefused(sessionID, cause)
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
		att.TurnCtxDone.Store(true)
	}
	a.closeDrainAttempt(att, err)
	if a.coord != nil {
		var awaiting *AwaitingAnswerError
		coord := a.coord
		switch {
		case errors.As(err, &awaiting):
			coord.SuspendAutoResume(call.SessionID)
		case att == nil && err != nil && providerTurnFailed(err, turnCtxDone):
			coord.NoteTurnFailed(call.SessionID)
		}
		if att == nil || att.Outcome == drainAttempted {
			coord.PushDeadlineOnTurnEnd(call.SessionID)
		}
	}
	if att != nil && att.Outcome == drainNotAttempted {
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
	if err == nil || !sessionprompt.IsDrainCallFrom(ctx) {
		return err
	}
	w.noteDrainRefused(sessionID, err)
	return &DrainNotAttemptedError{Err: err}
}

func (a *sessionAgent) decideDrainTurn(ctx context.Context, sessionID string, hasVisibleDebt bool) (bool, drainVerdict) {
	if !hasVisibleDebt {
		return false, drainVerdict{Kind: drainDeferred, Reason: "no visible debt"}
	}
	if a.coord == nil {
		return true, drainVerdict{Kind: drainAllow}
	}
	// The commit compares the bg-shell cap like a re-check (siteCommit: no
	// slot): a QUEUED Drain decides on debt its launch decision never saw (a
	// completion over the cap that landed after the Drain that owned the last
	// slot pulled), and it must not get a sixth automatic turn. A Drain's own
	// slot row is not an over-cap row, so it is never refused for the slot it
	// holds.
	v, _, err := a.coord.ArbiterVerdict(ctx, sessionID, uint8(siteCommit))
	if err != nil {
		return false, unreadablePolicyInput("launch decision input", sessionID, err)
	}
	dv := drainVerdictFromPort(v)
	if dv.Kind == drainAllow {
		// An allowed drain launch breaks await_tasks' sleep and cancels its
		// max_wait schedule (#1270). The wakeSession funnel (drainPolicy)
		// never sees an externally-driven session's drains — its loop
		// launches those itself — so this is the commit-side half of the
		// "every allowed launch clears" contract.
		a.coord.ClearSleepAll(sessionID)
	}
	return dv.Kind == drainAllow, dv
}

func SetDrainPacingForTest(retryAfter, refusalPause, recheckInterval time.Duration) (restore func()) {
	return drainctl.DrainPacingForTest(retryAfter, refusalPause, recheckInterval)
}

// turn: the affected row simply stays pending and is retried on the next
// pull (this turn's own step boundary, or a later turn).
func (a *sessionAgent) pullPendingNotices(ctx context.Context, sessionID string) (pulled []message.Message) {
	if sessionID == "" {
		return nil
	}
	// Batch-only pull: batch completion notices plus pending session
	// notices, both through the node-backed facts; no legacy void queries
	// and no async_jobs job-notice pull (the old table is gone).
	facts := coordinatorBatchCutoverFacts(a.coord)
	// claims[i] is the async_jobs claim id of pulled[i] -- always "" now:
	// batch notices and session_notices rows carry no legacy claim identity.
	// The reaction chain guard's key (#1113) stays: chainHasClaim("") is
	// false, so the "" claims are safe.
	claims := make([]string, 0, 8)
	bp, err := facts.PullBatchNotices(ctx, a.messages, sessionID)
	if err != nil {
		slog.Warn("notice pull: batch notice pull failed", "session_id", sessionID, "err", err)
	}
	for _, p := range bp {
		pulled = append(pulled, p.Message)
		claims = append(claims, "")
	}
	sp, err := facts.PullPendingSessionNotices(ctx, a.messages, sessionID, drainctl.BuildSessionNoticeMessageParams)
	if err != nil {
		slog.Warn("notice pull: batch session notice pull failed", "session_id", sessionID, "err", err)
	}
	for _, p := range sp {
		pulled = append(pulled, p.Message)
		claims = append(claims, "")
	}
	// Doc sec.3.4: moving a notice into history is progress for the
	// supervision countdown -- except a supervision check-in itself, which
	// must not reset the backoff it just grew, and a completion of the
	// reaction chain guard's own idle launches (#1113): a supervision tick
	// that drains such a completion must not reset the backoff either, or
	// the no-progress pause after 6 ticks never arrives.
	coord := a.coord
	for i, m := range pulled {
		if m.NoticeKind == sessionprompt.NoticeKindSupervision {
			continue
		}
		if claims[i] != "" && coord != nil && coord.ReactionChainHasClaim(sessionID, claims[i]) {
			continue
		}
		if coord != nil {
			coord.RecordProgress(sessionID)
		}
		break
	}
	return pulled
}

// pullPendingNoticesForStep is PrepareStep's step-boundary pull (doc
// sec.3.3): the same pull, at a point where the turn already runs.
func (a *sessionAgent) pullPendingNoticesForStep(ctx context.Context, sessionID string) []message.Message {
	return a.pullPendingNotices(ctx, sessionID)
}

// the session, so two processes can never write one session's messages.
func ForeignWorkspaceError(sess coresession.Session) error {
	return drainctl.ForeignWorkspaceError(sess)
}

// The Drain session policy (doc sec.3.4's "session policy" table): may
// sessionID's category get a Drain TURN at all. Consulted by wakeSession
// BEFORE a Drain is submitted and again by the Drain's own turn-start commit
// (agent_drain_decision.go). Accounting of an attempt lives in
// drain_attempt.go.
// drainVerdictKind is the outcome of a launch decision -- the agent package
// alias of the drainctl-owned leaf enum; the values live in
// runtime/exec/drainctl/turn_arbiter.go.
type drainVerdictKind = drainctl.DrainVerdictKind

// The launch-verdict kinds (drainctl owns the constants).
const (
	drainAllow    = drainctl.DrainAllow
	drainDeferred = drainctl.DrainDeferred
	drainPaced    = drainctl.DrainPaced
	drainStuck    = drainctl.DrainStuck
)

// drainVerdict is the package-local name for the moved launch-decision record.
type drainVerdict = drainctl.DrainVerdict

// The launch decision (R-ARB-2): readTurnFacts + decide is THE decision;
// drainPolicy/drainPermitted are now thin adapters translating the arbiter's
// verdict into the drainVerdict the internal callers and tests read. The old
// policy/gate split is gone -- the gate rows (8-10 of the arbiter table) are
// part of the same verdict. The reaction-chain guard's CONDITION is rule 5
// (computed in readTurnFacts); this file executes its marker.

// arbiterVerdict reads the facts for one launch site and decides. The site
// encodes `spent`: only siteFact (a committed fact's own launch) spends a
// bg-shell auto-resume slot.
func (w *drainComponent) arbiterVerdict(ctx context.Context, sessionID string, site LaunchSite) (Verdict, TurnFacts, error) {
	facts, err := w.readTurnFacts(ctx, sessionID, site)
	if err != nil {
		return Verdict{}, facts, err
	}
	return decide(facts), facts, nil
}

// drainVerdictOf maps an arbiter verdict onto the drainctl-owned kinds
// callers still speak; the mapping itself is drainctl-owned.
func drainVerdictOf(v Verdict) drainVerdict {
	return drainctl.DrainVerdictOf(v)
}

// drainVerdictFromPort rebuilds the arbiter verdict from the port's decision
// fields and maps it with drainVerdictOf, exactly as the direct call did.
func drainVerdictFromPort(v agentcall.CoordinatorVerdict) drainVerdict {
	return drainVerdictOf(Verdict{Kind: drainctl.VerdictKind(v.Kind()), Reason: v.Reason(), RecheckAt: v.RecheckAt()})
}

// unreadable wraps a read error as a fail-closed deferred verdict (worth a
// tick), as the old policy's per-input handler did; drainctl-owned
// (drainctl.UnreadablePolicyInput), same log line and outcome.
func unreadablePolicyInput(what string, sessionID string, err error) drainVerdict {
	return drainctl.UnreadablePolicyInput(what, sessionID, err)
}

// siteForSpent maps the legacy `spent` flag onto the launch site: a fact's
// own launch spent its slot at admission; everything else is a re-check.
func siteForSpent(spent bool) LaunchSite {
	if spent {
		return siteFact
	}
	return siteRelease
}

// drainPolicy is the launch verdict (the arbiter's), with the
// reaction-chain guard's marker executed on a web-driven session's first
// deferred read. spent: see siteForSpent.
func (w *drainComponent) drainPolicy(ctx context.Context, sessionID string, spent bool) drainVerdict {
	v, facts, err := w.arbiterVerdict(ctx, sessionID, siteForSpent(spent))
	if err != nil {
		return unreadablePolicyInput("launch decision input", sessionID, err)
	}
	if v.Kind == VNone && v.Reason == "no debt" {
		// The old policy refused a session regardless of debt (a foreign
		// driver, a suspension, a released child refused even a debt-free
		// one); the LAUNCHERS keep their own debt checks (drainDecision,
		// CLIScope, decideDrainTurn), so rule 1 never launched anything for
		// a debt-free session. Re-ask the arbiter without rule 1's
		// short-circuit to keep that contract; a VRun answer on a debt-free
		// session is still "nothing to launch" (allow).
		facts.Debt.PendingIncl = true
		v = decide(facts)
		if v.Kind == VRun {
			return drainVerdict{Kind: drainAllow}
		}
	}
	dv := drainVerdictOf(v)
	if dv.Kind == drainAllow {
		// Every allowed launch breaks await_tasks' sleep (rule 4b defers
		// while it holds, so an allow means the sleep is over or was broken
		// by a non-completion row); the max_wait schedule is cancelled with
		// it. This is the one funnel every Drain launch passes through.
		w.host.clearSleepAll(sessionID)
	}
	if dv.Kind == drainDeferred && dv.Reason == reactionChainReason &&
		!facts.Session.ChainNoticed && !facts.Session.ExternallyDrivenSelf {
		w.chainGuardMarker(ctx, sessionID)
	}
	return dv
}

// drainPermitted is THE launch predicate: the one arbiter verdict, mapped.
// spent: see siteForSpent.
func (w *drainComponent) drainPermitted(ctx context.Context, sessionID string, spent bool) drainVerdict {
	return w.drainPolicy(ctx, sessionID, spent)
}

// drainDecision reads the pending-inclusive debt and, when there is some,
// the launch verdict. No debt means nothing to launch for. spent: see
// drainPolicy.
func (w *drainComponent) drainDecision(ctx context.Context, sessionID string, spent bool) (debt bool, v drainVerdict, err error) {
	// The debt fact comes from batch node reads.
	debt, err = w.host.batchCutoverFacts().SelectedDebtExists(ctx, sessionID)
	if err != nil || !debt {
		return false, drainVerdict{}, err
	}
	return true, w.drainPolicy(ctx, sessionID, spent), nil
}

// isDurableDelegationChild reports whether sessionID was EVER created as a
// delegation target (session.CreateTaskSession sets ParentSessionID at
// creation, durably, unlike the in-memory toolwrap.Registry which
// releaseDriverIfScopeClosed tears down the instant scope closes).
//
// B3/C6 follow-up fix (docs/reviews/2026-09-29-async-phase4-round1.md, item
// 8): ParentSessionID alone is ambiguous -- `sessions fork --child` (the
// review's "sessions fork --parent" is the same knob under its CLI-flag
// name) sets the identical field on a session that was never a delegation
// target at all, for a wholly unrelated purpose (core.ForkOptions.
// ParentID). A forked child was, before this fix, refused Drain turns by
// the SAME branch a real released delegation child hits.
//
// The real signal doesn't need a schema change: every delegation claims an
// async_jobs row with Owner=ParentSessionID and ChildSessionID=the exact
// child session's id, independent of whether the job is still running -- the
// row outlives the delegation ending, until retention purges it. The Kind is
// deliberately NOT matched: runSubAgent claims JobKindAgent, agentic_fetch
// claims JobKindFetch, and any future kind that names a child session is a
// delegation target just the same. A plain fork never claims any async job
// at all, so no row naming it as ChildSessionID ever exists. This reuses
// the pre-existing ListAsyncJobsForOwner (the same query `sessions jobs`/
// `sessions why` already use -- no new SQL) instead of the coarser
// ParentSessionID-only check.
//
// Retention keeps the row while the child still has a running row or
// unreacted debt (R2A-10), so the signal outlives the child's work; a
// finished delegation's child past that window is an ordinary session again.
// An unreadable answer is not "not a child": drainPolicy fails closed.
func (w *drainComponent) isDurableDelegationChild(ctx context.Context, sessionID string) (bool, error) {
	if w.host.sessionsService() == nil {
		return false, nil
	}
	sess, err := w.host.sessionsService().Get(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if sess.ParentSessionID == "" {
		return false, nil
	}
	// Delegation identity composes the parent's live tree scan with the
	// ALL-roots history read -- a node under the parent naming this child,
	// live or historical. The historical half preserves the released-child
	// rule 7: the identity outlives the delegation's completion. An
	// unreadable read fails closed as delegated.
	return w.host.batchCutoverFacts().DelegationChildEver(ctx, sess.ParentSessionID, sessionID)
}

// sessionDebtIsBGShellOnly reports whether sessionID's ENTIRE current
// reaction debt (doc sec.3.4's predicate: wake=1/reacted=0/delivery<>'void',
// PENDING-INCLUSIVE -- same scope as ReactionDebtExists, deliberately NOT
// captureDebtSnapshot's delivery='done'-only scope, see below) consists of
// bg-shell-done notices (session.NoticeKindBGShellDone) and nothing else --
// no outstanding async-job debt, no other notice kind. Used by
// drainPolicy's B-dev6 fix (item 7) to re-apply the "Background
// shell: only with AutoResumeOnJobDone" policy-table row at release-recheck/
// 60s-pass time, which (unlike the direct hint-time call) has no ctx-carried
// signal of the debt's origin. false when there is no debt at all (nothing
// to gate) or on any job-id debt or mixed/non-bg-shell notice kind.
//
// W-DRAIN task A regression (found via the full internal/agent suite after
// narrowing captureDebtSnapshot to delivery='done' for C18/B-dev1): this
// used to read captureDebtSnapshot directly, which is CORRECT for settle-
// by-failure (only ever act on rows the failed/stuck turn actually saw) but
// WRONG here -- a release-recheck's own bg-shell notice is typically still
// delivery='pending' (not yet pulled by any turn) at the moment this policy
// check runs, so the done-only snapshot reported it as no debt at all,
// silently skipping the refusal gate and letting AutoResumeOnJobDone=off
// sessions get a Drain turn anyway. Reads the two tables directly instead,
// at the SAME pending-inclusive scope ReactionDebtExists uses.
func (w *drainComponent) sessionDebtIsBGShellOnly(ctx context.Context, sessionID string) (bgOnly bool, err error) {
	// The debt summary comes from batch node reads; the kind constants are
	// the same session_notices vocabulary.
	hasJobDebt, noticeKinds, err := w.host.batchCutoverFacts().PendingInclusiveDebtSummary(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if hasJobDebt || len(noticeKinds) == 0 {
		// Any outstanding async-job debt at all disqualifies -- this gate
		// only ever applies to a session whose ENTIRE debt is bg-shell
		// notices. No debt at all (nothing to gate) also disqualifies.
		return false, nil
	}
	for _, k := range noticeKinds {
		if k != session.NoticeKindBGShellDone {
			return false, nil
		}
	}
	return true, nil
}
