// Rerun truncation's async bookkeeping (doc sec.3.8, step 6): a history
// truncation deletes a tail of messages, and every async_jobs/session_notices
// row that tail touches must be reconciled in the SAME pass, before any new
// turn can start -- see RerunTruncate's own doc for the exact rules.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// RerunTruncate reconciles owner's async_jobs/session_notices rows against a
// Rerun's deleted message tail, in one transaction:
//
//  1. Repend: a row already delivery='done' whose OWN notice message
//     (notice_message_id) is in deletedMessageIDs goes back to
//     delivery='pending', reacted=0 -- the next turn on the new branch pulls
//     its result again (ASYNC-04). Same rule for session_notices.
//  2. Void: every async_jobs row whose OWNING tool call (tool_call_id) is in
//     deletedToolCallIDs is forced to delivery='void', regardless of its
//     current state/delivery -- including a still-'running' row, whose
//     eventual terminal transition preserves 'void' via its own CAS
//     (TransitionAsyncJobTerminalPreserveVoid), so a stop that raced or
//     failed can never resurrect a notice for a deleted call.
//
// Step 2 runs AFTER step 1 in the SAME transaction: a row whose own tool
// call AND whose earlier notice both fall in the deleted tail matches both
// queries, and void must win for it (doc sec.3.8: "these calls are gone").
// Caller (coordinator.RerunCleanupAsyncJobs) is expected to have already
// stopped any RUNNING row named by deletedToolCallIDs (job_kill semantics,
// recursively for a delegation's tree) before calling this -- this function
// only performs the delivery-state reconciliation, not the stop itself.
func (s *AsyncJobStore) RerunTruncate(ctx context.Context, owner string, deletedToolCallIDs, deletedMessageIDs []string) error {
	if owner == "" || (len(deletedToolCallIDs) == 0 && len(deletedMessageIDs) == 0) {
		return nil
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("async job store: rerun truncate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	now := time.Now().Unix()

	if len(deletedMessageIDs) > 0 {
		nullableIDs := make([]sql.NullString, len(deletedMessageIDs))
		for i, id := range deletedMessageIDs {
			nullableIDs[i] = sql.NullString{String: id, Valid: true}
		}
		if _, err := q.RependAsyncJobsByNoticeMessageIDs(ctx, db.RependAsyncJobsByNoticeMessageIDsParams{
			UpdatedAt: now, OwnerSessionID: owner, MessageIds: nullableIDs,
		}); err != nil {
			return fmt.Errorf("async job store: rerun truncate: repend async_jobs: %w", err)
		}
		if _, err := q.RependSessionNoticesByMessageIDs(ctx, db.RependSessionNoticesByMessageIDsParams{
			UpdatedAt: now, Owner: owner, MessageIds: nullableIDs,
		}); err != nil {
			return fmt.Errorf("async job store: rerun truncate: repend session_notices: %w", err)
		}
	}
	if len(deletedToolCallIDs) > 0 {
		if _, err := q.VoidAsyncJobsByToolCallIDs(ctx, db.VoidAsyncJobsByToolCallIDsParams{
			UpdatedAt: now, OwnerSessionID: owner, ToolCallIds: deletedToolCallIDs,
		}); err != nil {
			return fmt.Errorf("async job store: rerun truncate: void async_jobs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("async job store: rerun truncate: commit: %w", err)
	}
	return nil
}
