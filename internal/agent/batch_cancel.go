package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type batchCancelOutcome = batchmeta.CancelOutcome

func cancelBatchCall(ctx context.Context, runner batchRunnerHandle, root jobbatch.ID) batchCancelOutcome {
	return batchmeta.CancelCall(ctx, runner, root)
}
