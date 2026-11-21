package batchlife

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

func WithBatchManagedCall(ctx context.Context) context.Context {
	return tools.WithBatchManagedCall(ctx)
}

func IsBatchManagedCall(ctx context.Context) bool {
	return tools.IsBatchManagedCall(ctx)
}
