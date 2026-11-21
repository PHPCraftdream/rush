package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
)

func withBatchManagedCall(ctx context.Context) context.Context {
	return batchlife.WithBatchManagedCall(ctx)
}

func isBatchManagedCall(ctx context.Context) bool {
	return batchlife.IsBatchManagedCall(ctx)
}
