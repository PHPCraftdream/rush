// Full history wipe (`rush sessions reset`, R8A-3): ONE writer transaction
// deletes every message of a session and voids every async_jobs /
// session_notices row of that owner that could still reach the fresh
// history, so the "clean slate" is really one: no old notice is pulled into
// it and no done-but-unreacted debt from the wiped history survives. (Queued
// pending_injects rows go with their messages: ON DELETE CASCADE.)
//
// Running rows are never touched: their process is not the wiper's to stop
// (Rerun stops its own in-process jobs after commit, a CLI in another
// process cannot). The transaction refuses instead (ErrResetJobsRunning), so
// the check is atomic with the wipe.
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

// ErrResetJobsRunning means the owner still has running async_jobs rows:
// nothing was changed. Rows on a dead host become non-running through
// RecoverOwnerScope, which the caller runs first.
var ErrResetJobsRunning = errors.New("session reset: the session still has running async jobs")

// ResetOutcome is what one committed reset removed or voided.
type ResetOutcome struct {
	MessagesDeleted int
	JobsVoided      int64
	NoticesVoided   int64
}

// resetStepSeam is a test-only hook fired inside the reset transaction
// ("deleted": after the message delete; "voided": after the ledger voids,
// right before commit). An error aborts and rolls everything back. nil in
// production.
var resetStepSeam func(step string) error

func resetStep(step string) error {
	if resetStepSeam == nil {
		return nil
	}
	return resetStepSeam(step)
}

// ResetOwnerHistory wipes owner's messages and voids its pending and
// delivered-but-unreacted notice rows in one transaction; see the file doc.
// DeletedEvents are published only after commit.
func (s *AsyncJobStore) ResetOwnerHistory(ctx context.Context, messages message.Service, owner string) (ResetOutcome, error) {
	if owner == "" {
		return ResetOutcome{}, errors.New("session reset: owner is required")
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	running, err := q.ListRunningAsyncJobsForOwners(ctx, []string{owner})
	if err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: list running jobs: %w", err)
	}
	if len(running) > 0 {
		return ResetOutcome{}, fmt.Errorf("%w (%d running)", ErrResetJobsRunning, len(running))
	}

	// Ids are read inside the transaction (the writer connection holds the
	// write lock from BEGIN), so nothing can slip in before the delete.
	ids, err := sessionMessageIDsTx(ctx, tx, owner)
	if err != nil {
		return ResetOutcome{}, err
	}

	deleted, publish, err := messages.DeleteTx(ctx, tx, owner, ids)
	if err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: delete messages: %w", err)
	}
	if err := resetStep("deleted"); err != nil {
		return ResetOutcome{}, err
	}
	now := time.Now().Unix()
	out := ResetOutcome{MessagesDeleted: len(deleted)}
	if out.JobsVoided, err = q.VoidUndeliveredAsyncJobsForOwner(ctx, db.VoidUndeliveredAsyncJobsForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: void async_jobs: %w", err)
	}
	if out.NoticesVoided, err = q.VoidUndeliveredSessionNoticesForOwner(ctx, db.VoidUndeliveredSessionNoticesForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: void session_notices: %w", err)
	}
	if err := resetStep("voided"); err != nil {
		return ResetOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return ResetOutcome{}, fmt.Errorf("session reset: commit: %w", err)
	}
	publish()
	return out, nil
}

func sessionMessageIDsTx(ctx context.Context, tx *sql.Tx, owner string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE session_id = ?`, owner)
	if err != nil {
		return nil, fmt.Errorf("session reset: list messages: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("session reset: scan message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session reset: list messages: %w", err)
	}
	return ids, nil
}
