// Exact-incarnation rerun stop routing (R12-F, cutover acceptance 3): the
// batch-only post-commit executor stop (the retained rerun contract).
// The durable reconciliation is session.BatchStore.BatchRerunReconcile's
// single transaction (R10c); retirement is DURABLE and written THERE, in the
// truncation transaction (R12-F round 4: the live-root void runs in-tx, so
// it survives restarts and needs no in-memory registry). This goroutine runs
// only AFTER that transaction committed, from the Voided set it returned,
// and only stops executors: it never writes delivery state. Every entry
// carries its exact root_id, so stops are keyed by (owner, root_id) — never
// widened to a tool_call_id: a provider-reused call id can never cancel a
// NEW incarnation, and a late completion of a stopped old incarnation can
// only commit against its own (void-delivery or retired) root rows, never
// into the new one (I5).
package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

// BatchRerunStopper is the rerun stop surface (the only one; the legacy
// StopRerunJobs / session.VoidedAsyncJob contract is gone). It is an
// OPTIONAL interface so coordinator fakes can stay narrow: the rerun
// handler type-asserts it.
type BatchRerunStopper interface {
	BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode)
}

// batchRerunStopWorthy reports whether a voided incarnation's executor can
// still be stopped: the live states. A terminal incarnation is already
// settled; a stop would only chase dead rows.
func batchRerunStopWorthy(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// batchRerunRuntime unwraps the coordinator's bound batch runtime into the
// engine handle, owning host and batch store the stop needs. The binding is
// per-coordinator (BindCoordinatorBatchRuntime, batch_install_ingress.go);
// nil means not bound — the caller and its log treat that as "retire by
// delivery fencing only".
func batchRerunRuntime(c *coordinator) (runner batchRunnerHandle, host string, store *session.BatchStore) {
	bind := coordinatorBatchBindingFor(c)
	if bind == nil {
		return nil, "", nil
	}
	wi, ok := bind.ingress.(*batchWorkIngress)
	if !ok || wi == nil || wi.rt == nil {
		return nil, "", nil
	}
	if attach, ok := wi.rt.cfg.Attach.(batchAttachAdapter); ok {
		store = attach.store
	}
	return wi.rt.cfg.Runner, wi.rt.cfg.Host, store
}

// BatchRerunStopVoided implements the post-commit stop: stop exactly
// the local live engine roots the committed reconciliation voided, and stop
// every voided delegation's child tree (the retained transitive-stop shape). The durable void marks were already written by
// BatchRerunReconcile in its truncation transaction; nothing here touches
// delivery state.
// Best effort and idempotent: the rows are already void-delivery, and the
// re-read below is the repeated-stop suppression — after the first pass the
// stopped root settles terminal, so a second pass over the same Voided set
// is a no-op. A stop that misses (foreign host, engine gone, already
// retired) only means the executor's completion commits void instead of
// being stopped early.
func (c *coordinator) BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode) {
	if c == nil || len(voided) == 0 {
		return
	}
	runner, host, store := batchRerunRuntime(c)
	if runner == nil || host == "" {
		slog.Warn("coordinator.BatchRerunStopVoided: batch runtime not bound; voided incarnations retire by delivery fencing only",
			"session_id", sessionID, "voided", len(voided))
	}
	for i := range voided {
		v := voided[i]
		// Stop every voided delegation's child tree first, regardless of the
		// root's own state (the retained transitive-stop shape).
		if v.ChildSessionID != "" {
			c.stopTree(v.ChildSessionID)
		}
		if !batchRerunStopWorthy(v.State) {
			continue
		}
		if runner == nil {
			continue
		}
		if v.HostID != host {
			slog.Warn("coordinator.BatchRerunStopVoided: voided running incarnation lives on another host; its executor is not stopped, its completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "tool_call_id", v.ToolCallID, "host_id", v.HostID)
			continue
		}
		// Exact-incarnation guard AND repeated-stop suppression: re-read the
		// row and stop only while THIS root is still live. The durable void
		// already took delivery out of the pullable world; once the executor
		// confirms the stop the root settles terminal, so a second pass over
		// the same Voided set never cancels again — and a provider-reused
		// tool_call_id on a NEW live root can never widen this stop onto the
		// new incarnation.
		if store != nil {
			snap, err := store.Read(ctx, sessionID, v.RootID)
			if err != nil {
				slog.Warn("coordinator.BatchRerunStopVoided: voided incarnation row unreadable; skipping the stop",
					"session_id", sessionID, "root_id", v.RootID, "err", err)
				continue
			}
			if !batchRerunStopWorthy(snap.Root.State) {
				continue
			}
		}
		out := cancelBatchCall(ctx, runner, jobbatch.ID(v.RootID))
		if out.Err != nil {
			slog.Warn("coordinator.BatchRerunStopVoided: stop request failed; the completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "err", out.Err)
		}
	}
}
