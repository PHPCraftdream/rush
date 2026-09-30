// Rerun truncation (doc sec.3.8, step 6; docs/reviews/2026-09-29-async-
// phase4-round1-rerun-design.md): a Rerun commits exactly once. One writer
// transaction deletes the tail and the target message and reconciles every
// async_jobs/session_notices row against exactly the rows that transaction
// deleted. Before it commits nothing has changed; after it the rerun
// proceeds. Stopping the deleted tail's jobs happens strictly after commit
// (package agent, Coordinator.StopRerunJobs) from the Voided set returned
// here.
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

// rerunChunk bounds every IN-list of the reconciliation statements.
const rerunChunk = 500

// RerunTruncateParams names what a Rerun deletes: the target user message
// plus the tail of messages after it, all owned by Owner.
type RerunTruncateParams struct {
	Owner    string
	TargetID string
	TailIDs  []string
}

// VoidedAsyncJob is one async_jobs row the truncation voided, as it stood
// when voided: the caller stops the still-running ones and every delegation's
// child tree after the commit.
type VoidedAsyncJob struct {
	ToolCallID     string
	State          string
	ChildSessionID string
	HostID         string
}

// RerunTruncation is the committed outcome: the message rows actually
// deleted (target included) and the jobs voided.
type RerunTruncation struct {
	Deleted []message.Message
	Voided  []VoidedAsyncJob
}

// ErrRerunTargetGone means the target message was not among the rows the
// delete removed (already deleted by someone else, e.g. a concurrent rerun):
// nothing was changed.
var ErrRerunTargetGone = errors.New("rerun: target message no longer exists")

// rerunTruncateStepSeam is a test-only hook fired at named points inside the
// transaction ("deleted": after the message delete; "reconciled": after the
// ledger reconciliation, right before commit). An error aborts and rolls the
// whole transaction back. nil in production.
var rerunTruncateStepSeam func(step string) error

func rerunStep(step string) error {
	if rerunTruncateStepSeam == nil {
		return nil
	}
	return rerunTruncateStepSeam(step)
}

// TruncateForRerun runs the Rerun truncation transaction:
//
//  1. delete [target]+tail via message.Service.DeleteTx; the target must be
//     among the returned rows (else ErrRerunTargetGone, rollback);
//  2. from the rows ACTUALLY returned (target excluded) build the deleted
//     message id set and the tail's tool_call_id set -- a tail row someone
//     else already deleted is not reconciled;
//  3. re-pend delivered notices whose own message was deleted (ASYNC-04);
//     a wake_failed marker in the tail is voided instead of re-pended;
//  4. void every job whose "started" message (announce_message_id) was
//     deleted, then -- legacy arm, announce_message_id IS NULL -- every job
//     whose tool_call_id is in the deleted tail. Voids run AFTER the
//     re-pends: a row matching both must end void (doc sec.3.8).
//
// DeletedEvents are published only after commit.
func TruncateForRerun(ctx context.Context, sqlDB *sql.DB, messages message.Service, p RerunTruncateParams) (RerunTruncation, error) {
	if p.Owner == "" || p.TargetID == "" {
		return RerunTruncation{}, errors.New("rerun truncate: owner and target are required")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return RerunTruncation{}, fmt.Errorf("rerun truncate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	now := time.Now().Unix()

	ids := make([]string, 0, len(p.TailIDs)+1)
	ids = append(ids, p.TargetID)
	ids = append(ids, p.TailIDs...)
	deleted, publish, err := messages.DeleteTx(ctx, tx, p.Owner, ids)
	if err != nil {
		return RerunTruncation{}, fmt.Errorf("rerun truncate: delete messages: %w", err)
	}
	targetDeleted := false
	deletedIDs := make([]string, 0, len(deleted))
	var tailToolCallIDs []string
	for _, m := range deleted {
		if m.ID == p.TargetID {
			targetDeleted = true
			continue
		}
		deletedIDs = append(deletedIDs, m.ID)
		for _, tc := range m.ToolCalls() {
			tailToolCallIDs = append(tailToolCallIDs, tc.ID)
		}
	}
	if !targetDeleted {
		return RerunTruncation{}, ErrRerunTargetGone
	}
	if err := rerunStep("deleted"); err != nil {
		return RerunTruncation{}, err
	}

	var voided []VoidedAsyncJob
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		nullable := nullStrings(chunk)
		if _, err := q.RependAsyncJobsByNoticeMessageIDs(ctx, db.RependAsyncJobsByNoticeMessageIDsParams{
			UpdatedAt: now, OwnerSessionID: p.Owner, MessageIds: nullable,
		}); err != nil {
			return RerunTruncation{}, fmt.Errorf("rerun truncate: repend async_jobs: %w", err)
		}
		if _, err := q.RependSessionNoticesByMessageIDs(ctx, db.RependSessionNoticesByMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullable,
		}); err != nil {
			return RerunTruncation{}, fmt.Errorf("rerun truncate: repend session_notices: %w", err)
		}
		if _, err := q.VoidWakeFailedNoticesByMessageIDs(ctx, db.VoidWakeFailedNoticesByMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullable,
		}); err != nil {
			return RerunTruncation{}, fmt.Errorf("rerun truncate: void wake_failed notices: %w", err)
		}
	}
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		nullable := nullStrings(chunk)
		rows, err := q.VoidAsyncJobsByAnnounceMessageIDs(ctx, db.VoidAsyncJobsByAnnounceMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullable,
		})
		if err != nil {
			return RerunTruncation{}, fmt.Errorf("rerun truncate: void async_jobs by announce message: %w", err)
		}
		for _, r := range rows {
			voided = append(voided, VoidedAsyncJob{ToolCallID: r.ToolCallID, State: r.State, ChildSessionID: r.ChildSessionID.String, HostID: r.HostID})
		}
	}
	for _, chunk := range chunkStrings(tailToolCallIDs, rerunChunk) {
		rows, err := q.VoidAsyncJobsByToolCallIDs(ctx, db.VoidAsyncJobsByToolCallIDsParams{
			UpdatedAt: now, OwnerSessionID: p.Owner, ToolCallIds: chunk,
		})
		if err != nil {
			return RerunTruncation{}, fmt.Errorf("rerun truncate: void async_jobs by tool call: %w", err)
		}
		for _, r := range rows {
			voided = append(voided, VoidedAsyncJob{ToolCallID: r.ToolCallID, State: r.State, ChildSessionID: r.ChildSessionID.String, HostID: r.HostID})
		}
	}
	if err := rerunStep("reconciled"); err != nil {
		return RerunTruncation{}, err
	}
	if err := tx.Commit(); err != nil {
		return RerunTruncation{}, fmt.Errorf("rerun truncate: commit: %w", err)
	}
	publish()
	return RerunTruncation{Deleted: deleted, Voided: voided}, nil
}

// chunkStrings splits ids into slices of at most n; nil for an empty input.
func chunkStrings(ids []string, n int) [][]string {
	var out [][]string
	for start := 0; start < len(ids); start += n {
		out = append(out, ids[start:min(start+n, len(ids))])
	}
	return out
}

func nullStrings(ids []string) []sql.NullString {
	out := make([]sql.NullString, len(ids))
	for i, id := range ids {
		out[i] = sql.NullString{String: id, Valid: true}
	}
	return out
}
