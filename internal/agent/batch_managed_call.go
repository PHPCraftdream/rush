package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

func withBatchManagedCall(ctx context.Context) context.Context {
	return tools.WithBatchManagedCall(ctx)
}

func isBatchManagedCall(ctx context.Context) bool {
	return tools.IsBatchManagedCall(ctx)
}
