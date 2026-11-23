package agent

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// restoreFinalizationCallContext preserves trusted capabilities across mailbox
// handoffs without deriving authority from a session ID or driver generation.
func (a *sessionAgent) restoreFinalizationCallContext(ctx context.Context, call *SessionAgentCall) (context.Context, error) {
	if call == nil {
		return nil, gate.ErrFinalizationFactsUnavailable
	}
	guardCtx, cancel := context.WithTimeout(ctx, a.effectiveSessionPreambleMaxDuration())
	defer cancel()
	validated, err := call.FinalizationAuthorities.RestoreCall(guardCtx, call.SessionID, call.LogicalCallID)
	if err != nil {
		return nil, err
	}
	call.FinalizationAuthorities = session.CaptureAgentFinalizationContext(validated)
	// Preserve the caller's lifetime, not the bounded validation context.
	return call.FinalizationAuthorities.Restore(ctx)
}

// The three turnStream methods below moved verbatim out of the deleted
// finalization_execution.go (DM6 extracted the pure finalize cluster to
// runtime/batch/batchcore/finalize); they stay in the agent root because
// turnStream/sessionAgent are exec/agentrun domain (DM7 destination:
// finalization_methods.go).

func (ts *turnStream) beginFinalizationStep(ctx context.Context) error {
	ts.finalizationStamp = gate.Stamp{}
	if ts.finalization == nil {
		return nil
	}
	stamp, err := ts.finalization.Gate.BeginCandidate(ctx, ts.finalization.Read)
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	return nil
}

// markFinalizationStep runs under the assistant lock, before adding a final footer.
func (ts *turnStream) markFinalizationStep() error {
	stamp := ts.finalizationStamp
	if !stamp.BelongsTo(ts.finalization.Gate) || stamp.Serial() == 0 || ts.fenceCheckpoint == nil {
		return gate.ErrFinalizationFactsUnavailable
	}
	if err := ts.currentAssistant.MarkFinalizationCandidate(message.FinalizationIdentity{
		Key: ts.finalization.LogicalID, Generation: stamp.Epoch(),
		Revision: stamp.DurableRevision(), Serial: stamp.Serial(),
	}); err != nil {
		return err
	}
	ts.currentAssistant.CheckpointGeneration = ts.fenceCheckpoint()
	return nil
}

// sealFinalizationCandidate stays in the root because it has no production
// caller outside this package and its behavior is exercised by root white-box tests.
// It presents the persisted candidate to session-level TrySeal and must only
// run from the admission gate's publish step while the owner gate is held.
// Without an installed scope or facts reader it refuses without touching the database.
func sealFinalizationCandidate(ctx context.Context, execution *finalize.FinalizationExecution, messages message.Service, identity message.FinalizationIdentity, candidate gate.Candidate) (message.Message, error) {
	if execution == nil || execution.Scope.Store == nil || execution.LogicalID == "" {
		return message.Message{}, gate.ErrFinalizationFactsUnavailable
	}
	if execution.Scope.Store == nil || identity.Key != execution.Scope.Identity.LogicalCallID || execution.Scope.Identity.Generation <= 0 ||
		identity.Generation != uint64(execution.Scope.Identity.Generation) || identity.Revision > uint64(1<<63-1) {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	writer, ok := messages.(session.FinalizationSealWriter)
	if !ok {
		return message.Message{}, gate.ErrFinalizationFactsUnavailable
	}
	sealed, ok := session.CandidateOf(candidate.MessageID, candidate.Hash, identity)
	if !ok {
		return message.Message{}, gate.ErrFinalizationScopeChanged
	}
	outcome, err := execution.Scope.Store.TrySeal(ctx, execution.Scope.Identity, sealed, writer, execution.ScopeFacts)
	if err != nil {
		return message.Message{}, err
	}
	if outcome.Sealed || outcome.Accepted {
		return outcome.Message, nil
	}
	if outcome.Reason == session.AgentFinalizationSealCancelled {
		return message.Message{}, context.Canceled
	}
	// The owner composes or waits; never latch a failure for a deterministic
	// "not yet".
	return message.Message{}, gate.ErrFinalizationScopeChanged
}

// persistFinalizationCandidate replaces the plain step write for a candidate
// footer. A stale candidate is dropped (no candidate is recorded) and is not a
// turn error: the owner composes again.
func (ts *turnStream) persistFinalizationCandidate(snap message.Message) error {
	ts.finalizationCandidate = finalize.PersistedFinalizationCandidate{}
	finish := snap.FinishPart()
	if finish == nil || ts.finalization.Scope.Store == nil {
		return gate.ErrFinalizationFactsUnavailable
	}
	persisted, err := finalize.PersistFinalizationCandidate(ts.genCtx, ts.a.sessions, ts.a.messages, ts.finalization.Scope, finish.FinalizationIdentity(), snap)
	if errors.Is(err, gate.ErrFinalizationScopeChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	ts.finalizationCandidate = persisted
	return nil
}
