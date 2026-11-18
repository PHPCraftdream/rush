package session

import (
	finfacts "github.com/PHPCraftdream/rush/internal/session/batch/finfacts"
	finterm "github.com/PHPCraftdream/rush/internal/session/batch/finterm"
)

type FinalizationDescendantsFacts = finfacts.FinalizationDescendantsFacts

type FinalizationQueuedCallsFacts = finfacts.FinalizationQueuedCallsFacts

type FinalizationPhysicalHoldsFacts = finfacts.FinalizationPhysicalHoldsFacts

type FinalizationReactionDebtFacts = finfacts.FinalizationReactionDebtFacts

type FinalizationDescendantsReader = finfacts.FinalizationDescendantsReader

func NewFinalizationDescendantsReader() FinalizationDescendantsReader {
	return finfacts.NewFinalizationDescendantsReader()
}

func NewFinalizationQueuedCallsFacts() FinalizationQueuedCallsFacts {
	return finfacts.NewFinalizationQueuedCallsFacts()
}

func NewAgentFinalizationScopeReader(
	ownerSessionID string,
	descendants FinalizationDescendantsFacts,
	queued FinalizationQueuedCallsFacts,
	holds FinalizationPhysicalHoldsFacts,
	debt FinalizationReactionDebtFacts,
) finterm.AgentFinalizationScopeReader {
	return finfacts.NewAgentFinalizationScopeReader(ownerSessionID, descendants, queued, holds, debt)
}
