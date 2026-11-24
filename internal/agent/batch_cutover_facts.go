// BatchCutoverFacts is the R12-E single fact-source seam for the job-batches
// cutover: the terminal/debt/scope/reaction/wake questions the pre-cutover
// consumers direct at the async_jobs ledger. The node-backed implementation
// is the only source: it reads batch_nodes/batch outbox/session_notices only
// and never touches async_jobs (the legacy adapter is gone). Wake schedules
// and session driver markers are retained host infrastructure and stay on
// their existing readers.
package agent

import (
	"context"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// BatchCutoverFacts is retained as a root-package alias for its anchored
// coordinator, supervision, drain, adapter, and test callers.
type BatchCutoverFacts = agentcall.BatchCutoverFacts

// ErrBatchFactsSourceUnavailable marks a permanently unavailable batch
// facts node source (missing binding or missing node-source dependency).
// It is a terminal condition: callers must surface it as an error, never
// retry it.
var ErrBatchFactsSourceUnavailable = errBatchFactsUnavailable

// unknownBatchCutoverFacts is the flag-ON fail-safe: no node source could be
// constructed (no binding, no live reader). It never touches the ledger and
// answers every question conservatively: debt true, live true.
type unknownBatchCutoverFacts struct{}

func (unknownBatchCutoverFacts) SelectedDebtExists(context.Context, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) CaptureVisibleDebt(context.Context, string) (session.BatchDebtSnapshot, error) {
	return session.BatchDebtSnapshot{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) PendingInclusiveDebt(context.Context, string) (session.BatchPendingDebt, error) {
	return session.BatchPendingDebt{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) PendingInclusiveDebtSummary(context.Context, string) (bool, []string, error) {
	return true, nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) ReactedFailedTexts(context.Context, string) ([]session.BatchReactedFailedText, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveWorkSummary(context.Context, string) (int, int, error) {
	return 1, 1, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveScopeForOwners(_ context.Context, owners []string) ([]agentcall.BatchLiveScopeEntry, error) {
	out := make([]agentcall.BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		out = append(out, agentcall.BatchLiveScopeEntry{Owner: o, Live: true})
	}
	return out, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveDelegationChildOf(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) DelegationChildEver(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) LiveNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveDelegationNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) AttemptRows(context.Context, string, session.BatchDebtSnapshot, int) (agentcall.BatchAttemptRows, error) {
	return agentcall.BatchAttemptRows{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) IncrementWakeAttempts(context.Context, string, session.BatchDebtSnapshot) error {
	return errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) SettleReactedFailedWithMarker(context.Context, string, session.BatchDebtSnapshot, string) (int64, error) {
	return 0, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) DebtToolCallIDs(context.Context, session.BatchDebtSnapshot) []string {
	return nil
}

func (unknownBatchCutoverFacts) PullBatchNotices(context.Context, message.Service, string) ([]sessionnotice.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) PullPendingSessionNotices(context.Context, message.Service, string, func(sessionnotice.SessionNoticeRow) message.CreateMessageParams) ([]sessionnotice.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) HeldQuestions(context.Context, string) ([]sessionnotice.PendingNoticeDebt, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveHeldQuestions(context.Context, string) ([]sessionnotice.PendingNoticeDebt, error) {
	return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
}

// coordinatorBatchCutoverFactsBindings maps a *coordinator onto its
// explicitly bound fact source (R12-H wiring at install time).
var coordinatorBatchCutoverFactsBindings sync.Map

// BindCoordinatorBatchCutoverFacts binds one coordinator to the node-backed
// fact source. R12-H calls this beside BindCoordinatorBatchRuntime, e.g.
// BindCoordinatorBatchCutoverFacts(c, NewBatchCutoverNodeFacts(store, live,
// writer)). nil facts is ignored.
func BindCoordinatorBatchCutoverFacts(c Coordinator, facts BatchCutoverFacts) {
	if c == nil || facts == nil {
		return
	}
	coordinatorBatchCutoverFactsBindings.Store(c, facts)
}

// coordinatorBatchCutoverFacts returns the fact source for c: the bound node
// source, else the conservative unknown source (unwired coordinator: never
// idle, never "no debt"). There is no flag selector and no ledger adapter:
// batch node facts are the ONLY fact source.
func coordinatorBatchCutoverFacts(c *coordinator) BatchCutoverFacts {
	if c != nil {
		if v, ok := coordinatorBatchCutoverFactsBindings.Load(c); ok {
			return v.(BatchCutoverFacts)
		}
	}
	return unknownBatchCutoverFacts{}
}

// readTurnFactsBatch is readTurnFacts' only implementation: the same
// TurnFacts shape, with the DB half sourced from batch node reads and the
// in-process half (arbiter state, hint counter, external driver, gate) on
// the rehomed coordinator runtime state. Order is law: the debt rows
// first, then the memory half.
func (c *coordinator) readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts) (TurnFacts, error) {
	return newBatchCutoverGlueComponent(c).readTurnFactsBatch(ctx, facts, sessionID, f)
}
