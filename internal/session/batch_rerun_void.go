package session

import (
	"context"
	"fmt"
	"time"
)

// BatchVoidRetiredRoot durably quarantines a rerun-retired root: in one
// writer transaction it marks the root and all of its already-selected nodes
// delivery='void', wake=0, and synthetically selects+voids the root itself if
// no notice was selected yet. This is the durable replacement for the former
// in-memory fence registry (R12-F fixup): a later settlement that tries to
// select a notice for this root matches 0 rows (SelectBatchNodeNotice
// requires notice_selected=0), which the commit path tolerates as an already
// selected notice; and if the settlement lands first, its pending outbox row
// is voided here instead — BatchOutboxPending and BatchOutboxPull both
// require delivery='pending', so the row is never pulled. The race is free
// because the engine commit loop is serialized per Runner and this is the
// only competing writer: either the void mark lands first (0-row select,
// tolerated) or the settlement lands first (its pending row is voided).
// Both orders end delivery='void', wake=0: no outbox debt (BatchOutboxDebtNodes
// requires wake=1), no pull, and the mark survives a restart. State, cause,
// token and projection_sequence are untouched, so the settlement's own
// ProjectBatchNode fence (projection_sequence < sequence) stays unaffected.
// Best effort: 0 rows affected is a tolerated no-op (already void, terminal,
// or unknown root); only SQL errors are returned.
func (s *BatchStore) BatchVoidRetiredRoot(ctx context.Context, owner, rootID string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("batch void retired root: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	// (1) Live unselected root: a synthetic select + void. The notice_* SET
	// satisfies the notice_selected=1 CHECK branch (batch_nodes.sql:78-83:
	// kind/state/cause NOT NULL, sequence>0, ordinal>=0, delivery!='none');
	// 'stop' and 'cancelled' are CHECK-legal values.
	if _, err := tx.ExecContext(ctx, `UPDATE batch_nodes SET
		notice_selected = 1, notice_kind = 'root', notice_state = 'cancelled',
		notice_cause = 'stop', notice_sequence = 1, notice_ordinal = 0,
		delivery = 'void', wake = 0, updated_at = ?
	WHERE owner_session_id = ? AND root_id = ? AND node_id = root_id
		AND notice_selected = 0
		AND state IN ('pending', 'starting', 'running', 'cancelling')`, now, owner, rootID); err != nil {
		return fmt.Errorf("batch void retired root: select live root: %w", err)
	}
	// (2) Already-selected root not yet void: keep the settlement's notice
	// identity, only take delivery out of the pullable world.
	if _, err := tx.ExecContext(ctx, `UPDATE batch_nodes SET
		delivery = 'void', wake = 0, updated_at = ?
	WHERE owner_session_id = ? AND root_id = ? AND node_id = root_id
		AND notice_selected = 1 AND delivery NOT IN ('void')`, now, owner, rootID); err != nil {
		return fmt.Errorf("batch void retired root: void selected root: %w", err)
	}
	// (3) Already-selected non-root nodes of this root: same quarantine.
	if _, err := tx.ExecContext(ctx, `UPDATE batch_nodes SET
		delivery = 'void', wake = 0, updated_at = ?
	WHERE owner_session_id = ? AND root_id = ? AND node_id != root_id
		AND notice_selected = 1 AND delivery NOT IN ('void')`, now, owner, rootID); err != nil {
		return fmt.Errorf("batch void retired root: void selected nodes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("batch void retired root: %w", err)
	}
	return nil
}
