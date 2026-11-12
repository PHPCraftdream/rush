package agent

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// finalizationExecution belongs to one logical call, not its inherited children.
type finalizationExecution struct {
	sessionID string
	logicalID string
	gate      *finalizationGate
	read      func(context.Context) (finalizationFacts, error)
	// scope is the durable record candidates are persisted against; without
	// it a candidate cannot be persisted and the step fails closed.
	scope finalizationPublicationScope
	// scopeFacts is the durable scope-facts reader TrySeal consults; without it
	// the seal refuses (fail-closed) until a later slice installs the reader.
	scopeFacts session.AgentFinalizationScopeReader
}

type finalizationExecutionContextKey struct{}

func withFinalizationExecution(ctx context.Context, execution *finalizationExecution) (context.Context, error) {
	if execution == nil || execution.sessionID == "" || execution.logicalID == "" || execution.gate == nil || execution.gate.epoch == 0 {
		return nil, ErrFinalizationFactsUnavailable
	}
	return context.WithValue(ctx, finalizationExecutionContextKey{}, execution), nil
}

func finalizationExecutionFor(ctx context.Context, call SessionAgentCall) *finalizationExecution {
	if ctx == nil {
		return nil
	}
	execution, _ := ctx.Value(finalizationExecutionContextKey{}).(*finalizationExecution)
	if execution == nil || execution.sessionID != call.SessionID || execution.logicalID != call.LogicalCallID {
		return nil
	}
	return execution
}

func (ts *turnStream) beginFinalizationStep(ctx context.Context) error {
	ts.finalizationStamp = finalizationStamp{}
	if ts.finalization == nil {
		return nil
	}
	stamp, err := ts.finalization.gate.beginCandidate(ctx, ts.finalization.read)
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	return nil
}

// markFinalizationStep runs under the assistant lock, before adding a final footer.
func (ts *turnStream) markFinalizationStep() error {
	stamp := ts.finalizationStamp
	if stamp.owner != ts.finalization.gate || stamp.serial == 0 || ts.fenceCheckpoint == nil {
		return ErrFinalizationFactsUnavailable
	}
	if err := ts.currentAssistant.MarkFinalizationCandidate(message.FinalizationIdentity{
		Key: ts.finalization.logicalID, Generation: stamp.epoch,
		Revision: stamp.durableRevision, Serial: stamp.serial,
	}); err != nil {
		return err
	}
	ts.currentAssistant.CheckpointGeneration = ts.fenceCheckpoint()
	return nil
}

// persistFinalizationCandidate replaces the plain step write for a candidate
// footer. A stale candidate is dropped (no candidate is recorded) and is not a
// turn error: the owner composes again.
func (ts *turnStream) persistFinalizationCandidate(snap message.Message) error {
	ts.finalizationCandidate = persistedFinalizationCandidate{}
	finish := snap.FinishPart()
	if finish == nil || ts.finalization.scope.store == nil {
		return ErrFinalizationFactsUnavailable
	}
	persisted, err := persistFinalizationCandidate(ts.genCtx, ts.a.sessions, ts.a.messages, ts.finalization.scope, finish.FinalizationIdentity(), snap)
	if errors.Is(err, ErrFinalizationScopeChanged) {
		return nil
	}
	if err != nil {
		return err
	}
	ts.finalizationCandidate = persisted
	return nil
}

// isFinalizationCandidate reports an unfinished provisional footer.
func isFinalizationCandidate(m message.Message) bool {
	finish := m.FinishPart()
	return finish != nil && finish.Partial && finish.FinalizationKey != ""
}
