// `sessions gc --jobs-older-than` support (phase-4 step 7, doc sec.3.7):
// the same terminal/delivered-or-voided retention predicate the 60s pass
// applies via PurgeExpired, exposed with a CALLER-CHOSEN age instead of the
// fixed AsyncDataRetentionAge, for an operator-triggered one-shot purge.
// LIVE rows (state='running') are never touched -- that scoping lives in
// the SQL itself (PurgeAsyncJobsOlderThan/CountAsyncJobsOlderThan), not
// here, so a row of an unknown-liveness host is never at risk either: a
// 'running' row is excluded outright, and a terminal row's host does not matter
// -- except an unnamed job_kill row (delivery='done', no result message),
// which is kept until its host is dead and recovery re-pends it (R5A-1).
package session

import (
	"context"
	"fmt"
	"time"
)

// CountJobsOlderThan reports how many async_jobs/session_notices rows
// PurgeJobsOlderThan(age) would delete, without deleting anything --
// `sessions gc --jobs-older-than --dry-run`.
func (s *AsyncJobStore) CountJobsOlderThan(ctx context.Context, age time.Duration) (jobs, notices int64, err error) {
	cutoff := time.Now().Add(-age).Unix()
	jobs, err = s.q.CountAsyncJobsOlderThan(ctx, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("async job store: count jobs older than: async_jobs: %w", err)
	}
	notices, err = s.q.CountSessionNoticesOlderThan(ctx, cutoff)
	if err != nil {
		return jobs, 0, fmt.Errorf("async job store: count jobs older than: session_notices: %w", err)
	}
	return jobs, notices, nil
}

// PurgeJobsOlderThan deletes terminal, delivered-or-voided async_jobs/
// session_notices rows older than age (doc sec.3.7) -- `sessions gc
// --jobs-older-than`. Never touches unreacted debt or an unnamed job_kill row
// (R5A-1). Returns the number of rows deleted from each table.
func (s *AsyncJobStore) PurgeJobsOlderThan(ctx context.Context, age time.Duration) (jobs, notices int64, err error) {
	cutoff := time.Now().Add(-age).Unix()
	jobs, err = s.q.PurgeAsyncJobsOlderThan(ctx, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("async job store: purge jobs older than: async_jobs: %w", err)
	}
	notices, err = s.q.PurgeSessionNoticesOlderThan(ctx, cutoff)
	if err != nil {
		return jobs, 0, fmt.Errorf("async job store: purge jobs older than: session_notices: %w", err)
	}
	return jobs, notices, nil
}
