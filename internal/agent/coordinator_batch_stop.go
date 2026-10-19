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
	bind := coordinatorBatchBindingFor(c)
	var runtimeRunner batchRunnerHandle
	_, localHost, _ := batchRerunRuntime(c)
	if bind == nil || bind.controls == nil {
		runtimeRunner, _, _ = batchRerunRuntime(c)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	for _, target := range targets {
		if target.Host != "" && (localHost == "" || target.Host != localHost) {
			continue
		}
		if bind != nil && bind.controls != nil {
			if _, err := bind.controls.Stop(stopCtx, target.Owner, target.Root, target.Root); err != nil {
				slog.Error("coordinator.Cancel: native batch stop failed", "owner", target.Owner, "root", target.Root, "err", err)
			}
			continue
		}
		if runtimeRunner == nil {
			continue
		}
		if out := cancelBatchCall(stopCtx, runtimeRunner, jobbatch.ID(target.Root)); out.Err != nil {
			slog.Error("coordinator.Cancel: batch stop failed", "owner", target.Owner, "root", target.Root, "err", out.Err)
		}
	}
}
