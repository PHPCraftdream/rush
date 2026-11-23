package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

func cancelBatchCall(ctx context.Context, runner batchcore.RunnerHandle, root jobbatch.ID) batchmeta.CancelOutcome {
	return batchmeta.CancelCall(ctx, runner, root)
}
