package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Keep the agent-side names while the remaining finalization wiring stays
// here; these aliases bridge the established agent surface to finalize.
type (
	FinalizationAuthorityRegistry = finalize.FinalizationAuthorityRegistry
)

func NewFinalizationAuthorityRegistry() *FinalizationAuthorityRegistry {
	return finalize.NewFinalizationAuthorityRegistry()
}

func finalizationExecutionFor(ctx context.Context, call SessionAgentCall) *finalize.FinalizationExecution {
	return finalize.FinalizationExecutionFor(ctx, call)
}

func isFinalizationCandidate(m message.Message) bool {
	return finalize.IsFinalizationCandidate(m)
}
