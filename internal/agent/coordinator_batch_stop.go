package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// stopBatchOwners durably fences local batch roots before issuing best-effort
// executor stops. The fence is authoritative even when cancellation fails.
func (c *coordinator) stopBatchOwners(ids []string) {
	store := batchCutoverReactionStore(c)
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	targets, err := store.BatchStopOwners(ctx, ids)
	cancel()
	if err != nil {
		slog.Error("coordinator.Cancel: failed to fence batch roots", "owners", ids, "err", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	runtimeRunner, localHost, _ := batchRerunRuntime(c)
	if runtimeRunner == nil {
		return
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	for _, target := range targets {
		if target.Host != "" && (localHost == "" || target.Host != localHost) {
			continue
		}
		if out := cancelBatchCall(stopCtx, runtimeRunner, jobbatch.ID(target.Root)); out.Err != nil {
			// A failed cancellation must leave the durable request pending for retry.
			slog.Error("coordinator.Cancel: batch stop failed", "owner", target.Owner, "root", target.Root, "err", out.Err)
			continue
		}
		answerCtx, answerCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.BatchAnswerOwnerStop(answerCtx, target); err != nil {
			slog.Error("coordinator.Cancel: failed to answer batch stop", "owner", target.Owner, "root", target.Root, "request_id", target.RequestID, "err", err)
		}
		answerCancel()
	}
}
