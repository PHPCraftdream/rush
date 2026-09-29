// RerunTruncateAsyncJobs wires the Rerun handler (internal/server) into the
// ledger's Rerun-scoped stop (work_ledger_rerun.go) and the store's Rerun
// DB reconciliation (internal/session/async_job_rerun.go).
package agent

import (
	"context"
	"fmt"
)

// RerunTruncateAsyncJobs implements Coordinator.RerunTruncateAsyncJobs. See
// that method's doc. A coordinator with no async job store wired (asyncJobs
// nil, or its store nil -- an unusual but not-impossible construction) is a
// no-op: there is nothing durable to reconcile.
func (c *coordinator) RerunTruncateAsyncJobs(ctx context.Context, sessionID string, deletedToolCallIDs, deletedMessageIDs []string) error {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return nil
	}
	if len(deletedToolCallIDs) == 0 && len(deletedMessageIDs) == 0 {
		return nil
	}
	// Step 1: stop every RUNNING row the deleted tail names, recursively
	// through a delegation's tree (like Stop) -- doc sec.3.8's "job_kill
	// semantics". affectedTree is sessionID's own deleted tool calls PLUS
	// every descendant session a stopped delegation's tree reached.
	affectedTree := c.asyncJobs.stopToolCallsForRerun(sessionID, deletedToolCallIDs)
	if len(affectedTree) > 0 {
		// Doc sec.3.4/3.8: zero the wake bit on every already-terminal
		// (pending/done) debt row across the whole newly-stopped subtree --
		// the same race Stop's own Cancel closes, applied to a Rerun-scoped
		// stop instead of a whole-session one.
		if err := c.asyncJobs.store.SetWakeZeroForOwners(ctx, affectedTree); err != nil {
			return fmt.Errorf("rerun truncate async jobs: zero wake for stopped subtree: %w", err)
		}
	}
	// Step 2: the DB-level void/repend reconciliation, scoped to sessionID
	// itself -- a delegation's OWN child session never appears directly in
	// sessionID's message history, so only sessionID's own rows can match
	// either the notice_message_id or tool_call_id sets built from its
	// deleted tail.
	if err := c.asyncJobs.store.RerunTruncate(ctx, sessionID, deletedToolCallIDs, deletedMessageIDs); err != nil {
		return fmt.Errorf("rerun truncate async jobs: %w", err)
	}
	return nil
}
