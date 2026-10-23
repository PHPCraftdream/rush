// Batch history consumers, store level (R10c chunk 3, inert): owner/session
// deletion cascade (D-R10c-1), selected-node delivery reconciliation against
// deleted history (D-R10c-3) and legacy bound-notice disposal (D-R10c-5).
// Nothing here is called by production code until the R12 cutover.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// BatchOwnerDeleteOutcome is what one committed owner deletion removed.
type BatchOwnerDeleteOutcome struct {
	MessagesDeleted    int
	NodesDeleted       int64 // batch_nodes rows, all incarnations of the owner
	NoticesVoided      int64 // legacy undelivered session_notices
	SchedulesCancelled int64
}

// BatchOwnerDeleteHistory removes the owner's messages and batch node rows in
// ONE transaction; batch_control_requests cascade by FK, and the session row
// itself is deleted by session.Service.Delete (whose FKs cascade any
// remainder). D-R10c-1: mirrors the old code's observed behavior — the
// deletion refuses NOTHING, not even a live nonterminal root; a live Runner
// writing after the commit loses its rows and fails Project with
// ErrBatchProjectionConflict (containment by failure, never a synthesized
// settlement). Contrast BatchResetOwnerHistory, which refuses live roots:
// reset is a user-facing clean-slate operation, deletion is disposal of a
// session that is going away regardless. DeletedEvents publish only after
// commit.
func (s *BatchStore) BatchOwnerDeleteHistory(ctx context.Context, messages message.Service, owner string) (BatchOwnerDeleteOutcome, error) {
	if owner == "" {
		return BatchOwnerDeleteOutcome{}, errors.New("batch owner delete: owner is required")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	ids, err := sessionMessageIDsTx(ctx, tx, owner)
	if err != nil {
		return BatchOwnerDeleteOutcome{}, err
	}
	deleted, publish, err := messages.DeleteTx(ctx, tx, owner, ids)
	if err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: delete messages: %w", err)
	}
	now := time.Now().Unix()
	out := BatchOwnerDeleteOutcome{MessagesDeleted: len(deleted)}
	if out.NodesDeleted, err = q.BatchResetDeleteOwnerNodes(ctx, owner); err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: delete batch nodes: %w", err)
	}
	if out.NoticesVoided, err = q.VoidUndeliveredSessionNoticesForOwner(ctx, db.VoidUndeliveredSessionNoticesForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: void session_notices: %w", err)
	}
	if out.SchedulesCancelled, err = q.CancelAllWakeSchedulesForOwner(ctx, db.CancelAllWakeSchedulesForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: cancel wake schedules: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return BatchOwnerDeleteOutcome{}, fmt.Errorf("batch owner delete: commit: %w", err)
	}
	publish()
	return out, nil
}

// BatchHistoryReconcileDelivery reconciles every selected node whose notice
// message is among deletedNoticeMessageIDs (a set a reset/rerun/message
// deletion just removed) to a consistent delivery value (D-R10c-3): 'done'
// and 'pending' become a fresh pending with the dead message reference and
// attempt counters cleared, so no node stays 'done' for a nonexistent message
// and no debt reader keys a deleted row; 'void' stays void (disposal is
// final) and unselected rows ('none') are untouched. Runs outside any
// message transaction: the caller commits the deletion first.
func (s *BatchStore) BatchHistoryReconcileDelivery(ctx context.Context, owner string, deletedNoticeMessageIDs []string) (int64, error) {
	if owner == "" {
		return 0, errors.New("batch reconcile: owner is required")
	}
	var total int64
	for _, chunk := range chunkStrings(deletedNoticeMessageIDs, rerunChunk) {
		n, err := db.New(s.writer).BatchHistoryRependNodesByDeletedNoticeMessages(ctx, db.BatchHistoryRependNodesByDeletedNoticeMessagesParams{
			UpdatedAt: time.Now().Unix(), Owner: owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return total, fmt.Errorf("batch reconcile: repend nodes: %w", err)
		}
		total += n
	}
	return total, nil
}

// BatchDisposeLegacyBoundNotices voids the legacy bound session_notices rows
// of one root's tool call that a selected node notice has superseded
// (D-R10c-5): delivery IN ('pending','done') AND wake = 1 AND reacted = 0,
// so disposal happens exactly once — a second call is a 0-row no-op, an
// already-void notice is never re-touched, and a reacted notice is left as
// history. Wake is zeroed with the void so debt readers cannot count the row
// again. Returns the rows this call disposed.
func (s *BatchStore) BatchDisposeLegacyBoundNotices(ctx context.Context, owner, toolCallID string) (int64, error) {
	if owner == "" || toolCallID == "" {
		return 0, errors.New("batch dispose: owner and tool call are required")
	}
	if _, err := db.New(s.writer).BatchListBoundNoticesByToolCallID(ctx, db.BatchListBoundNoticesByToolCallIDParams{
		Owner: owner, ToolCallID: sql.NullString{String: toolCallID, Valid: true},
	}); err != nil {
		return 0, fmt.Errorf("batch dispose: list bound notices: %w", err)
	}
	n, err := db.New(s.writer).BatchDisposeBoundNoticesByToolCallID(ctx, db.BatchDisposeBoundNoticesByToolCallIDParams{
		UpdatedAt: time.Now().Unix(), Owner: owner, ToolCallID: sql.NullString{String: toolCallID, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("batch dispose: void bound notices: %w", err)
	}
	return n, nil
}
