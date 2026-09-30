// Notice-row identity for the web bg-shell auto-turn cap (docs/async-invariants.md
// ASYNC-09; plan amendment (ad)): the insert that returns the new row's id and
// the pending-inclusive debt read that names its notice rows. Additive
// siblings of InsertSessionNotice (notice_pull.go) and
// PendingInclusiveDebtSummary (async_job_reaction.go), same scope and rules.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// PendingNoticeDebt is one notice row of a pending-inclusive debt: its id
// (AUTOINCREMENT, never reused) and Kind.
type PendingNoticeDebt struct {
	ID   int64
	Kind string
}

// InsertSessionNoticeReturningID is InsertSessionNotice that also returns the
// new row's id. A failed insert returns (0, err): no row exists.
func (s *AsyncJobStore) InsertSessionNoticeReturningID(ctx context.Context, owner, kind, text string, wake bool, jobToolCallID string) (int64, error) {
	wakeInt := int64(0)
	if wake {
		wakeInt = 1
	}
	var jobToolCallIDParam sql.NullString
	if jobToolCallID != "" {
		jobToolCallIDParam = sql.NullString{String: jobToolCallID, Valid: true}
	}
	now := time.Now().Unix()
	row, err := s.q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: owner, Kind: kind, Text: text, Wake: wakeInt,
		JobToolCallID: jobToolCallIDParam, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

// PendingInclusiveDebtRows is PendingInclusiveDebtSummary with the notice rows
// named by id, oldest first: whether owner has ANY job-id debt row, and every
// notice-debt row (wake=1/reacted=0/delivery<>'void') with its Kind.
func (s *AsyncJobStore) PendingInclusiveDebtRows(ctx context.Context, owner string) (hasJobDebt bool, notices []PendingNoticeDebt, err error) {
	jobs, err := s.q.ListAsyncJobsForOwner(ctx, owner)
	if err != nil {
		return false, nil, fmt.Errorf("async job store: pending-inclusive debt rows: async_jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Wake != 0 && j.Reacted == 0 && j.Delivery != "void" && j.Announced != 0 {
			hasJobDebt = true
			break
		}
	}
	rows, err := s.q.ListSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return false, nil, fmt.Errorf("async job store: pending-inclusive debt rows: session_notices: %w", err)
	}
	for _, n := range rows {
		if n.Wake != 0 && n.Reacted == 0 && n.Delivery != "void" {
			notices = append(notices, PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
		}
	}
	return hasJobDebt, notices, nil
}
