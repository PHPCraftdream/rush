package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
)

func armBatchQuestionTurnContext(ctx context.Context, call SessionAgentCall) context.Context {
	return batchmeta.ArmBatchQuestionTurnContext(ctx, call, state.WithQuestionContext, withBatchAgentQuestionBinding)
}
