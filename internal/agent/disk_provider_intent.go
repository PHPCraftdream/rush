package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
)

type ExecutionProviderIntent = batchlife.ExecutionProviderIntent

func WithExecutionProviderIntent(ctx context.Context, sessionID string, intent ExecutionProviderIntent) context.Context {
	return batchlife.WithExecutionProviderIntent(ctx, sessionID, intent)
}
