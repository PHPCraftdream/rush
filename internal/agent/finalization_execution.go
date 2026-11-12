package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/message"
)

// finalizationExecution belongs to one logical call, not its inherited children.
type finalizationExecution struct {
	sessionID string
	logicalID string
	gate      *finalizationGate
	read      func(context.Context) (finalizationFacts, error)
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
