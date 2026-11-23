package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// ErrFinalizationComposeUnavailable refuses to rebuild the answer when there
// is nothing recomposable to rebuild; it is fail-closed, never a silent pass.
var ErrFinalizationComposeUnavailable = errors.New("finalization compose unavailable")

// FinalizationComposeInput carries what a recomposition needs: the turn, its
// execution, the cloned provisional answer, and a fresh stamp from
// gate.BeginCandidate.
type FinalizationComposeInput struct {
	Turn        *turnStream
	Execution   *finalize.FinalizationExecution
	Provisional message.Message
	Stamp       gate.Stamp
}

// FinalizationComposeFunc rebuilds the provisional answer under the fresh
// stamp and returns the persisted candidate; nil deps.Compose uses the
// built-in defaultComposeFinalizationCandidate.
type FinalizationComposeFunc func(context.Context, FinalizationComposeInput) (finalize.PersistedFinalizationCandidate, error)

// composeFinalizationCandidate rebuilds the provisional answer under a fresh
// stamp and writes the fresh stamp and persisted candidate back to the turn.
// FinalizeTurn runs after the callback sequence, so these fields are only
// touched here and by the loop itself.
func composeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalize.FinalizationExecution, deps FinalizationWaitDeps) error {
	ts.mu.Lock()
	if ts.currentAssistant == nil {
		ts.mu.Unlock()
		return fmt.Errorf("%w: no provisional assistant answer to recompose", ErrFinalizationComposeUnavailable)
	}
	snapshot := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	stamp, err := execution.Gate.BeginCandidate(ctx, execution.Read)
	if err != nil {
		return fmt.Errorf("finalization fresh stamp failed: %w", err)
	}
	var persisted finalize.PersistedFinalizationCandidate
	if deps.Compose != nil {
		persisted, err = deps.Compose(ctx, FinalizationComposeInput{
			Turn: ts, Execution: execution, Provisional: snapshot, Stamp: stamp,
		})
	} else {
		persisted, err = defaultComposeFinalizationCandidate(ctx, ts, execution, snapshot, stamp)
	}
	if err != nil {
		return err
	}
	ts.finalizationStamp = stamp
	ts.finalizationCandidate = persisted
	return nil
}

// defaultComposeFinalizationCandidate re-marks the cloned provisional answer
// with the fresh stamp identity and persists it; creds, role, tools and
// budgets are preserved by construction because it is the same turnStream.
func defaultComposeFinalizationCandidate(ctx context.Context, ts *turnStream, execution *finalize.FinalizationExecution, snapshot message.Message, stamp gate.Stamp) (finalize.PersistedFinalizationCandidate, error) {
	finish := snapshot.FinishPart()
	if finish == nil || !finish.Partial || finish.FinalizationKey != execution.LogicalID {
		return finalize.PersistedFinalizationCandidate{}, fmt.Errorf("%w: the provisional answer carries no recomposable candidate footer", ErrFinalizationComposeUnavailable)
	}
	identity := message.FinalizationIdentity{
		Key: execution.LogicalID, Generation: stamp.Epoch(),
		Revision: stamp.DurableRevision(), Serial: stamp.Serial(),
	}
	if err := snapshot.MarkFinalizationCandidate(identity); err != nil {
		return finalize.PersistedFinalizationCandidate{}, err
	}
	return finalize.PersistFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.Scope, identity, snapshot)
}

// finalizationPublishClosure builds the publication gate.TrySeal calls back:
// it presents the turn's persisted candidate, verifies the accepted row, and
// reports a durable scope change through the scopeChanged flag so the loop can
// wait instead of recomposing. It runs only under the gate's mutex.
func finalizationPublishClosure(ts *turnStream, execution *finalize.FinalizationExecution, scopeChanged *bool) func(context.Context, gate.Candidate) error {
	return func(ctx context.Context, candidate gate.Candidate) error {
		persisted := ts.finalizationCandidate
		if candidate.MessageID != persisted.Candidate.MessageID || candidate.Hash != persisted.Candidate.Hash {
			*scopeChanged = true
			return gate.ErrFinalizationScopeChanged
		}
		accepted, err := finalize.PublishFinalizationCandidate(ctx, ts.a.sessions, ts.a.messages, execution.Scope, persisted.Identity, candidate)
		if err != nil {
			if errors.Is(err, gate.ErrFinalizationScopeChanged) {
				*scopeChanged = true
			}
			return err
		}
		// A successful publication clears the flag: the flag survives only
		// while the durable side keeps answering "not yet".
		finish := accepted.FinishPart()
		if !accepted.IsFinished() || accepted.ID != candidate.MessageID || accepted.SessionID != ts.call.SessionID ||
			finish == nil || finish.Reason != message.FinishReasonEndTurn ||
			finish.FinalizationKey != persisted.Identity.Key ||
			finish.FinalizationGeneration != persisted.Identity.Generation ||
			finish.FinalizationRevision != persisted.Identity.Revision ||
			finish.FinalizationSerial != persisted.Identity.Serial ||
			finish.FinalizationHash != candidate.Hash {
			return fmt.Errorf("%w: publisher returned no matching accepted row", gate.ErrFinalizationFactsUnavailable)
		}
		ts.mu.Lock()
		*ts.currentAssistant = accepted
		ts.mu.Unlock()
		return nil
	}
}
