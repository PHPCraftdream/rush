// Atomic reset refusal (R10c, D-R10c-2): one writer transaction checks that
// the owner owns NO nonterminal root incarnation and then deletes every
// message and batch row of that owner. A Reserve cannot interleave: the
// nonterminal scan and the deletion share the transaction, so a root that
// appears before the check refuses the reset and a root that appears after
// survives intact. Refusal writes NOTHING. Mirrors async_job_reset.go; the
// batch model has no wake schedules of its own yet, so the legacy
// CancelAllWakeSchedulesForOwner runs unchanged (D-R10c-5 open decision).
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// BatchResetOutcome is what one committed batch reset removed.
type BatchResetOutcome struct {
	MessagesDeleted    int
	NodesDeleted       int64 // batch_nodes rows, all incarnations of the owner
	NoticesVoided      int64 // legacy undelivered session_notices
	SchedulesCancelled int64
}

// batchResetRefusalState names the first nonterminal root incarnation of the
// owner, "stopped but awaiting termination" included: a cancelling
// projection stays nonterminal until the confirmed report (I8).
func batchResetRefusalState(n db.BatchNode) (string, bool) {
	if batchReadTerminal(n.State) {
		return "", false
	}
	return n.State, true
}

// batchResetLiveRoots is the tx-internal refusal scan. Inside the caller's
// transaction the writer connection already holds the write lock, so no
// Reserve can slip a root in between this scan and the deletion.
func batchResetLiveRoots(ctx context.Context, q *db.Queries, owner string) ([]db.BatchNode, error) {
	roots, err := q.BatchHistoryRootsForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var live []db.BatchNode
	for _, r := range roots {
		if _, non := batchResetRefusalState(r); non {
			live = append(live, r)
		}
	}
	return live, nil
}

// BatchResetOwnerHistory wipes the owner's messages and batch node rows in
// one transaction; see the file doc. DeletedEvents publish only after commit.
func (s *BatchStore) BatchResetOwnerHistory(ctx context.Context, messages message.Service, owner string) (BatchResetOutcome, error) {
	if owner == "" {
		return BatchResetOutcome{}, errors.New("batch reset: owner is required")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	live, err := batchResetLiveRoots(ctx, q, owner)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: scan live roots: %w", err)
	}
	if len(live) > 0 {
		first := live[0]
		return BatchResetOutcome{}, &BatchResetRefusal{Owner: owner, RootID: first.RootID, ToolCallID: first.ToolCallID.String, State: first.State}
	}

	ids, err := sessionMessageIDsTx(ctx, tx, owner)
	if err != nil {
		return BatchResetOutcome{}, err
	}
	deleted, publish, err := messages.DeleteTx(ctx, tx, owner, ids)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: delete messages: %w", err)
	}
	now := time.Now().Unix()
	out := BatchResetOutcome{MessagesDeleted: len(deleted)}
	if out.NodesDeleted, err = q.BatchResetDeleteOwnerNodes(ctx, owner); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: delete batch nodes: %w", err)
	}
	if out.NoticesVoided, err = q.VoidUndeliveredSessionNoticesForOwner(ctx, db.VoidUndeliveredSessionNoticesForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: void session_notices: %w", err)
	}
	if out.SchedulesCancelled, err = q.CancelAllWakeSchedulesForOwner(ctx, db.CancelAllWakeSchedulesForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: cancel wake schedules: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: commit: %w", err)
	}
	publish()
	return out, nil
}
