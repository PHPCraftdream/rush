// WakeScheduleStore is the durable wake-schedule store (stage 4a,
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md sec.4,
// contract 2026-09-27-wake-tools-contract.md sec.1/sec.6). Storage and
// lifecycle only: the worker/timer that polls ClaimDue is stage 4b. Every
// mutation is a lease-keyed CAS, so two concurrent schedulers can never
// double-fire an occurrence (SCHED-1/SCHED-2 in docs/async-invariants.md).
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

// WakeKind is the wake_schedules.kind vocabulary.
type WakeKind string

const (
	WakeKindOnce WakeKind = "once"
	WakeKindLoop WakeKind = "loop"
)

// Contract limits (2026-09-27-wake-tools-contract.md sec.6).
const (
	MaxActiveWakeSchedulesPerSession = 20
	MaxWakeMessageLength             = 4000 // runes
	MinWakeOnceDelay                 = 5 * time.Second
	MinLoopInterval                  = 300 * time.Second
	MaxLoopRuns                      = 10000
	MaxWakeHorizon                   = 100 * 365 * 24 * time.Hour // unit-mistake guard only
	// WakeLeaseTTL is how long a ClaimDue lease lasts by default: longer
	// than any single fire's transactional work, short enough that a dead
	// scheduler's rows are recovered on the next sweep.
	WakeLeaseTTL = 5 * time.Minute

	// NoticeKindWakeFired is the session_notices kind for a fired
	// occurrence (existing transport, no new mechanism).
	NoticeKindWakeFired = "wake_fired"
)

// ErrWakeScheduleLimit is the 20-active-schedules-per-session refusal
// (SCHED-4).
var ErrWakeScheduleLimit = errors.New("maximum number of active schedules (20) reached for this session")

// ErrWakeScheduleNotOwned means the caller is not the owning session.
var ErrWakeScheduleNotOwned = errors.New("wake schedule does not belong to this session")

// WakeScheduleStore reads and writes wake_schedules through the shared
// connection pool. It holds no host lock: schedules are claimed by lease
// columns, not OS locks.
type WakeScheduleStore struct {
	sqlDB *sql.DB
	q     *db.Queries
}

// NewWakeScheduleStore wires the store onto an already-migrated DB handle.
func NewWakeScheduleStore(conn *sql.DB) *WakeScheduleStore {
	return &WakeScheduleStore{sqlDB: conn, q: db.New(conn)}
}

// CreateWakeScheduleParams is CreateSchedule's input. RunAt is the first
// scheduled firing (UTC). For a loop, Every must be set; MaxRuns == 0 means
// unbounded (Until still applies); Until == nil means unbounded.
type CreateWakeScheduleParams struct {
	Owner   string
	Kind    WakeKind
	Message string
	RunAt   time.Time
	Every   time.Duration
	MaxRuns int
	Until   *time.Time
}

// Validate enforces the contract limits; the schedule's own clock is the
// caller-supplied now.
func (p CreateWakeScheduleParams) Validate(now time.Time) error {
	if p.Owner == "" {
		return errors.New("wake schedule: owner session is required")
	}
	if n := utf8.RuneCountInString(p.Message); n == 0 || n > MaxWakeMessageLength {
		return fmt.Errorf("wake schedule: message must be 1..%d characters (got %d)", MaxWakeMessageLength, n)
	}
	if p.RunAt.Sub(now) > MaxWakeHorizon {
		return errors.New("wake schedule: scheduled time is over 100 years away")
	}
	switch p.Kind {
	case WakeKindOnce:
		if p.RunAt.Sub(now) < MinWakeOnceDelay {
			return fmt.Errorf("wake schedule: scheduled time must be at least %s in the future", MinWakeOnceDelay)
		}
	case WakeKindLoop:
		if p.Every < MinLoopInterval {
			return fmt.Errorf("wake schedule: every must be at least %s", MinLoopInterval)
		}
		if p.MaxRuns < 0 || p.MaxRuns > MaxLoopRuns {
			return fmt.Errorf("wake schedule: max_runs must be between 1 and %d", MaxLoopRuns)
		}
		if p.Until != nil {
			if p.Until.Sub(now) < MinWakeOnceDelay {
				return fmt.Errorf("wake schedule: until must be at least %s in the future", MinWakeOnceDelay)
			}
			if p.Until.Sub(now) > MaxWakeHorizon {
				return errors.New("wake schedule: until is over 100 years away")
			}
		}
	default:
		return fmt.Errorf("wake schedule: unknown kind %q", p.Kind)
	}
	return nil
}

// CreateSchedule validates and inserts a schedule, enforcing the
// 20-active-per-session limit in the same transaction as the insert
// (SCHED-4).
func (s *WakeScheduleStore) CreateSchedule(ctx context.Context, p CreateWakeScheduleParams, now time.Time) (db.WakeSchedule, error) {
	if err := p.Validate(now); err != nil {
		return db.WakeSchedule{}, err
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return db.WakeSchedule{}, fmt.Errorf("wake schedule: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := s.q.WithTx(tx)

	count, err := q.CountActiveWakeSchedulesForOwner(ctx, p.Owner)
	if err != nil {
		return db.WakeSchedule{}, fmt.Errorf("wake schedule: count active: %w", err)
	}
	if count >= MaxActiveWakeSchedulesPerSession {
		return db.WakeSchedule{}, ErrWakeScheduleLimit
	}

	ts := now.Unix()
	row, err := q.InsertWakeSchedule(ctx, db.InsertWakeScheduleParams{
		ID:             "wake_" + uuid.NewString(),
		OwnerSessionID: p.Owner,
		Kind:           string(p.Kind),
		Message:        p.Message,
		NextRunAt:      p.RunAt.UTC().Unix(),
		EveryMs:        p.Every.Milliseconds(),
		MaxRuns:        wakeNullMaxRuns(p.MaxRuns),
		UntilAt:        wakeNullUntil(p.Until),
		CreatedAt:      ts,
		UpdatedAt:      ts,
	})
	if err != nil {
		return db.WakeSchedule{}, fmt.Errorf("wake schedule: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.WakeSchedule{}, fmt.Errorf("wake schedule: commit: %w", err)
	}
	return row, nil
}

// ListSchedules returns the owner's schedules, soonest first (wake_list).
func (s *WakeScheduleStore) ListSchedules(ctx context.Context, owner string) ([]db.WakeSchedule, error) {
	return s.q.ListWakeSchedulesForOwner(ctx, owner)
}

// CountActive returns the owner's active schedule count (limit checks,
// wake_list summaries).
func (s *WakeScheduleStore) CountActive(ctx context.Context, owner string) (int64, error) {
	return s.q.CountActiveWakeSchedulesForOwner(ctx, owner)
}

// CancelSchedule moves an active schedule to cancelled. Idempotent: an
// already done/cancelled row is a nil no-op; a foreign owner is refused
// (SCHED-5). Cancelling a leased row also drops the lease so a pending
// claim can never fire a cancelled schedule.
func (s *WakeScheduleStore) CancelSchedule(ctx context.Context, owner, id string, now time.Time) error {
	row, err := s.q.GetWakeSchedule(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // nothing to cancel
	}
	if err != nil {
		return fmt.Errorf("wake schedule: get: %w", err)
	}
	if row.OwnerSessionID != owner {
		return ErrWakeScheduleNotOwned
	}
	if _, err := s.q.CancelWakeSchedule(ctx, db.CancelWakeScheduleParams{
		UpdatedAt: now.Unix(), ID: id, OwnerSessionID: owner,
	}); err != nil {
		return fmt.Errorf("wake schedule: cancel: %w", err)
	}
	return nil
}

// CancelAllForOwner cancels every active schedule of the owner
// (`sessions reset` sibling of ResetOwnerHistory's void passes).
func (s *WakeScheduleStore) CancelAllForOwner(ctx context.Context, owner string, now time.Time) (int64, error) {
	n, err := s.q.CancelAllWakeSchedulesForOwner(ctx, db.CancelAllWakeSchedulesForOwnerParams{
		UpdatedAt: now.Unix(), OwnerSessionID: owner,
	})
	if err != nil {
		return 0, fmt.Errorf("wake schedule: cancel all: %w", err)
	}
	return n, nil
}

// ClaimDue atomically leases up to limit due active schedules to leaseOwner
// until now+leaseTTL and returns the claimed rows. The read, the per-row
// lease CAS and the commit are one transaction (the writer connection holds
// the write lock from BEGIN), so concurrent callers get disjoint sets
// (SCHED-2); a row whose lease expired is claimable again.
func (s *WakeScheduleStore) ClaimDue(ctx context.Context, leaseOwner string, now time.Time, limit int, leaseTTL time.Duration) ([]db.WakeSchedule, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("wake schedule: claim begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := s.q.WithTx(tx)

	due, err := q.ListDueWakeSchedules(ctx, db.ListDueWakeSchedulesParams{
		NextRunAt:      now.Unix(),
		LeaseExpiresAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		Limit:          int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("wake schedule: list due: %w", err)
	}
	nowTS := now.Unix()
	lease := sql.NullString{String: leaseOwner, Valid: true}
	expiry := sql.NullInt64{Int64: now.Add(leaseTTL).Unix(), Valid: true}
	claimed := make([]db.WakeSchedule, 0, len(due))
	for _, row := range due {
		n, err := q.ClaimWakeScheduleLease(ctx, db.ClaimWakeScheduleLeaseParams{
			LeaseOwner: lease, LeaseExpiresAt: expiry, UpdatedAt: nowTS,
			ID: row.ID, LeaseExpiresAt_2: sql.NullInt64{Int64: nowTS, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("wake schedule: claim lease: %w", err)
		}
		if n == 1 {
			// The SELECT row predates the lease write; re-read so callers
			// see the lease they just won.
			fresh, err := q.GetWakeSchedule(ctx, row.ID)
			if err != nil {
				return nil, fmt.Errorf("wake schedule: re-read claimed: %w", err)
			}
			claimed = append(claimed, fresh)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("wake schedule: claim commit: %w", err)
	}
	return claimed, nil
}

// RecoverExpiredLeases releases every active row whose lease expired
// without firing (SCHED-2). Firing clears the lease in the same
// transaction as the state change, so recovery never duplicates an
// occurrence.
func (s *WakeScheduleStore) RecoverExpiredLeases(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.RecoverExpiredWakeLeases(ctx, db.RecoverExpiredWakeLeasesParams{
		UpdatedAt: now.Unix(), LeaseExpiresAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("wake schedule: recover leases: %w", err)
	}
	return n, nil
}

// NextDue returns the earliest next_run_at among active schedules, or nil
// when none are left -- the scheduler timer's sleep target.
func (s *WakeScheduleStore) NextDue(ctx context.Context) (*time.Time, error) {
	v, err := s.q.NextDueWakeScheduleAt(ctx)
	if err != nil {
		return nil, fmt.Errorf("wake schedule: next due: %w", err)
	}
	if v == nil {
		return nil, nil
	}
	sec, ok := v.(int64)
	if !ok {
		return nil, nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t, nil
}

// OpenWakeSchedule is one ACTIVE once schedule as the CLI lifetime and the
// `sessions` readers see it (stage 5a): a wakein/wakeon timer that still
// holds its session's `rush run` open.
type OpenWakeSchedule struct {
	ID        string
	Message   string
	NextRunAt time.Time
}

// OpenOnceWakeSchedules lists owner's ACTIVE once schedules, soonest first.
// Loop schedules are not open work (the CLI cancels them at scope close),
// so they are not reported.
func OpenOnceWakeSchedules(ctx context.Context, s *WakeScheduleStore, owner string) ([]OpenWakeSchedule, error) {
	rows, err := s.ListSchedules(ctx, owner)
	if err != nil {
		return nil, err
	}
	var open []OpenWakeSchedule
	for _, row := range rows {
		if row.Kind != string(WakeKindOnce) || row.State != "active" {
			continue
		}
		open = append(open, OpenWakeSchedule{
			ID: row.ID, Message: row.Message,
			NextRunAt: time.Unix(row.NextRunAt, 0).UTC(),
		})
	}
	sort.Slice(open, func(i, j int) bool { return open[i].NextRunAt.Before(open[j].NextRunAt) })
	return open, nil
}
