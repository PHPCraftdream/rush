package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/config"
)

// The alias keeps the coordinator's context-key filter on the same key type.
type batchQuestionRouteContextKey = batchmeta.QuestionRouteContextKey

func withBatchQuestionRoute(ctx context.Context, route bool) context.Context {
	return batchmeta.WithBatchQuestionRoute(ctx, route)
}

func batchQuestionRouteFrom(ctx context.Context) (bool, bool) {
	return batchmeta.BatchQuestionRouteFrom(ctx)
}

func applyBatchQuestionRouteFloor(ctx context.Context, agent config.Agent) config.Agent {
	return batchmeta.ApplyBatchQuestionRouteFloor(ctx, agent)
}

func stripAskQuestionTools(list []fantasy.AgentTool) []fantasy.AgentTool {
	return batchmeta.StripAskQuestionTools(list)
}

func armBatchQuestionTurnContext(ctx context.Context, call SessionAgentCall) context.Context {
	return batchmeta.ArmBatchQuestionTurnContext(ctx, call, withBatchAgentQuestionContext, withBatchAgentQuestionBinding)
}
