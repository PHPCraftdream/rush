package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// sealFinalizationCandidate is gate.trySeal's durable publication: it presents
// the persisted candidate to the session-level TrySeal. It must only run from
// the admission gate's publish step while the owner gate is held; without an
// installed scope or facts reader it refuses without touching the database.
func sealFinalizationCandidate(ctx context.Context, execution *finalizationExecution, messages message.Service, identity message.FinalizationIdentity, candidate finalizationCandidate) (message.Message, error) {
	if execution == nil || execution.scope.store == nil || execution.logicalID == "" {
		return message.Message{}, ErrFinalizationFactsUnavailable
	}
	if !execution.scope.matches(identity) {
		return message.Message{}, ErrFinalizationScopeChanged
	}
	writer, ok := messages.(session.FinalizationSealWriter)
	if !ok {
		return message.Message{}, ErrFinalizationFactsUnavailable
	}
	sealed, ok := session.CandidateOf(candidate.messageID, candidate.hash, identity)
	if !ok {
		return message.Message{}, ErrFinalizationScopeChanged
	}
	outcome, err := execution.scope.store.TrySeal(ctx, execution.scope.identity, sealed, writer, execution.scopeFacts)
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
	return message.Message{}, ErrFinalizationScopeChanged
}
