package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
)

type ExecutionProviderIntent = batchlife.ExecutionProviderIntent

const (
	ExecutionProviderOmitted = batchlife.ExecutionProviderOmitted
	ExecutionProviderCustom  = batchlife.ExecutionProviderCustom
	ExecutionProviderOS      = batchlife.ExecutionProviderOS
)

func WithExecutionProviderIntent(ctx context.Context, sessionID string, intent ExecutionProviderIntent) context.Context {
	return batchlife.WithExecutionProviderIntent(ctx, sessionID, intent)
}

func executionProviderIntentFrom(ctx context.Context, sessionID string) ExecutionProviderIntent {
	return batchlife.ExecutionProviderIntentFrom(ctx, sessionID)
}
