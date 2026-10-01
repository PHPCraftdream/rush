// Wake-schedule occurrence firing (stage 4a): FireOccurrence advances ONE
// occurrence and enqueues the session notice in a single transaction. The
// lease-keyed CAS is the idempotency mechanism (SCHED-1): a replayed fire
// for the same lease loses the CAS and the transaction rolls back, so
// schedule_id#occurrence can never yield two notices.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

func wakeNullMaxRuns(n int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(n), Valid: n > 0}
}

func wakeNullUntil(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().Unix(), Valid: true}
}

// WakeFire is FireOccurrence's outcome.
type WakeFire struct {
	Fired      bool // this caller's lease won the occurrence
	Done       bool // the schedule reached a terminal state with this fire
	Occurrence int64
	NextRun    *time.Time // next scheduled firing for a loop that continues
}

// nextLoopRun returns the first schedule boundary strictly after now,
// counting from the SCHEDULED time sched (never from the fire time), so a
// downtime gap is skipped, not caught up (SCHED-3): at most one immediate
// firing happens after downtime, and the cadence stays on the original
// grid.
func nextLoopRun(sched int64, everyMs int64, now time.Time) time.Time {
	if everyMs <= 0 {
		return time.Unix(sched, 0)
	}
	deltaMs := now.UnixMilli() - sched*1000
	if deltaMs < 0 {
		deltaMs = 0
	}
	steps := deltaMs/everyMs + 1 // boundary is strictly > now
	return time.UnixMilli(sched*1000 + steps*everyMs)
}

// FireOccurrence fires the occurrence held by leaseOwner for the schedule
// id at time now: once -> done; loop -> occurrence+1 and next_run_at moved
// to the next schedule boundary (finishing instead when max_runs is
// reached or the next boundary would cross until_at). The state change and
// the durable wake notice (session_notices, kind wake_fired, wake=1) are
// one transaction, so a fire either fully happened or did not. A stale or
// expired lease returns Fired=false and changes nothing.
func (s *WakeScheduleStore) FireOccurrence(ctx context.Context, id, leaseOwner string, now time.Time) (WakeFire, error) {
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return WakeFire{}, fmt.Errorf("wake schedule: fire begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := s.q.WithTx(tx)

	row, err := q.GetWakeSchedule(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return WakeFire{}, nil // cascaded away mid-lease: nothing to fire at
	}
	if err != nil {
		return WakeFire{}, fmt.Errorf("wake schedule: fire get: %w", err)
	}
	if row.State != "active" ||
		!row.LeaseOwner.Valid || row.LeaseOwner.String != leaseOwner ||
		!row.LeaseExpiresAt.Valid || row.LeaseExpiresAt.Int64 < now.Unix() {
		return WakeFire{}, nil // lease lost, expired or already fired
	}

	fireNo := row.Occurrence + 1
	ts := now.Unix()
	fire := WakeFire{Fired: true, Occurrence: fireNo}

	switch WakeKind(row.Kind) {
	case WakeKindOnce:
		if _, err := q.CompleteOnceWakeSchedule(ctx, db.CompleteOnceWakeScheduleParams{
			UpdatedAt: ts, ID: id, LeaseOwner: row.LeaseOwner,
		}); err != nil {
			return WakeFire{}, fmt.Errorf("wake schedule: complete once: %w", err)
		}
		fire.Done = true
		if err := insertWakeFiredNotice(ctx, q, row, fireNo, ts, nil); err != nil {
			return WakeFire{}, err
		}
	case WakeKindLoop:
		next := nextLoopRun(row.NextRunAt, row.EveryMs, now)
		finished := (row.MaxRuns.Valid && fireNo >= row.MaxRuns.Int64) ||
			(row.UntilAt.Valid && next.Unix() > row.UntilAt.Int64)
		if finished {
			if _, err := q.FinishLoopWakeSchedule(ctx, db.FinishLoopWakeScheduleParams{
				UpdatedAt: ts, ID: id, LeaseOwner: row.LeaseOwner,
			}); err != nil {
				return WakeFire{}, fmt.Errorf("wake schedule: finish loop: %w", err)
			}
			fire.Done = true
		} else {
			if _, err := q.AdvanceLoopWakeOccurrence(ctx, db.AdvanceLoopWakeOccurrenceParams{
				NextRunAt: next.Unix(), UpdatedAt: ts, ID: id, LeaseOwner: row.LeaseOwner,
			}); err != nil {
				return WakeFire{}, fmt.Errorf("wake schedule: advance loop: %w", err)
			}
			t := next.UTC()
			fire.NextRun = &t
		}
		if err := insertWakeFiredNotice(ctx, q, row, fireNo, ts, fire.NextRun); err != nil {
			return WakeFire{}, err
		}
	default:
		return WakeFire{}, fmt.Errorf("wake schedule: unknown kind %q", row.Kind)
	}

	if err := tx.Commit(); err != nil {
		return WakeFire{}, fmt.Errorf("wake schedule: fire commit: %w", err)
	}
	return fire, nil
}

// insertWakeFiredNotice enqueues the durable wake notice through the
// existing session_notices transport. The occurrence number in the text is
// the human-visible form of the schedule_id#occurrence idempotency key.
func insertWakeFiredNotice(ctx context.Context, q *db.Queries, row db.WakeSchedule, fireNo, ts int64, next *time.Time) error {
	var text string
	switch WakeKind(row.Kind) {
	case WakeKindOnce:
		text = fmt.Sprintf("Wake %s fired (scheduled for %s): %s",
			row.ID, time.Unix(row.NextRunAt, 0).UTC().Format(time.RFC3339), row.Message)
	case WakeKindLoop:
		nextPart := "none"
		if next != nil {
			nextPart = next.Format(time.RFC3339)
		}
		// §5.1's "of {max_runs_or_infinity}": the bound the loop counts
		// toward, so the model can tell how far along the loop is.
		maxPart := "infinity"
		if row.MaxRuns.Valid {
			maxPart = strconv.FormatInt(row.MaxRuns.Int64, 10)
		}
		text = fmt.Sprintf("Loop %s occurrence %d of %s fired: %s\n\nNext occurrence: %s.",
			row.ID, fireNo, maxPart, row.Message, nextPart)
	}
	if _, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: row.OwnerSessionID, Kind: NoticeKindWakeFired, Text: text,
		Wake: 1, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		return fmt.Errorf("wake schedule: insert notice: %w", err)
	}
	return nil
}
