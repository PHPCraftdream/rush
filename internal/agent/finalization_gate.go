package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	sessionfacade "github.com/PHPCraftdream/rush/internal/session"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Keep the agent-side names while the remaining finalization wiring stays
// here; these aliases bridge the established agent surface to finalize.
type (
	finalizationFacts     = gate.Facts
	finalizationStamp     = gate.Stamp
	finalizationCandidate = gate.Candidate
	finalizationGate      = gate.Gate

	finalizationExecution          = finalize.FinalizationExecution
	finalizationPublicationScope   = finalize.FinalizationPublicationScope
	persistedFinalizationCandidate = finalize.PersistedFinalizationCandidate
	finalizationCancelRegistry     = finalize.FinalizationCancelRegistry
	FinalizationCancelLookup       = finalize.FinalizationCancelLookup
	FinalizationAuthorityRegistry  = finalize.FinalizationAuthorityRegistry
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

	ErrFinalizationSettlerUnavailable = finalize.ErrFinalizationSettlerUnavailable
)

func finalizationLogicalIdentity(snapshot session.AgentFinalizationContext, sessionID, logicalID string) (string, error) {
	return finalize.FinalizationLogicalIdentity(snapshot, sessionID, logicalID)
}

func NewFinalizationAuthorityRegistry() *FinalizationAuthorityRegistry {
	return finalize.NewFinalizationAuthorityRegistry()
}

func persistFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope finalizationPublicationScope, identity message.FinalizationIdentity, candidate message.Message) (persistedFinalizationCandidate, error) {
	return finalize.PersistFinalizationCandidate(ctx, sessions, messages, scope, identity, candidate)
}

func publishFinalizationCandidate(ctx context.Context, sessions session.Service, messages message.Service, scope finalizationPublicationScope, identity message.FinalizationIdentity, candidate finalizationCandidate) (message.Message, error) {
	return finalize.PublishFinalizationCandidate(ctx, sessions, messages, scope, identity, candidate)
}

func settleFinalizationDebts(ctx context.Context, settler sessionfacade.AgentFinalizationDebtSettler, ownerSessionID string) error {
	return finalize.SettleFinalizationDebts(ctx, settler, ownerSessionID)
}

func withFinalizationExecution(ctx context.Context, execution *finalizationExecution) (context.Context, error) {
	return finalize.WithFinalizationExecution(ctx, execution)
}

func finalizationExecutionFor(ctx context.Context, call SessionAgentCall) *finalizationExecution {
	return finalize.FinalizationExecutionFor(ctx, call)
}

func isFinalizationCandidate(m message.Message) bool {
	return finalize.IsFinalizationCandidate(m)
}

func newFinalizationCancelRegistry() *finalizationCancelRegistry {
	return finalize.NewFinalizationCancelRegistry()
}

func NewFinalizationOnCancelHook(lookup FinalizationCancelLookup) func(sessionID string) {
	return finalize.NewFinalizationOnCancelHook(lookup)
}

func NewFinalizationRecoverScopeHook(messages message.Service) func(context.Context, *finalizationExecution) (session.AgentFinalizationRecovery, error) {
	return finalize.NewFinalizationRecoverScopeHook(messages)
}

func newFinalizationGate(epoch uint64) *finalizationGate { return gate.New(epoch) }
