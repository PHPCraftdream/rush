package agent

import "github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"

// Keep the agent-side names while the remaining finalization wiring stays here.
type (
	finalizationFacts     = gate.Facts
	finalizationStamp     = gate.Stamp
	finalizationCandidate = gate.Candidate
	finalizationGate      = gate.Gate
)

const (
	finalizationWaitWork      = gate.WaitWork
	finalizationPauseQuestion = gate.PauseQuestion
	finalizationCompose       = gate.Compose
	finalizationAccepted      = gate.Accepted
)

var (
	ErrFinalizationFactsUnavailable = gate.ErrFinalizationFactsUnavailable
	ErrFinalizationSealed           = gate.ErrFinalizationSealed
	ErrFinalizationScopeChanged     = gate.ErrFinalizationScopeChanged
)

func newFinalizationGate(epoch uint64) *finalizationGate { return gate.New(epoch) }
