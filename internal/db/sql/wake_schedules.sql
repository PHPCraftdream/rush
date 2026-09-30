-- Wake schedules (stage 4a, wake_schedules table). Storage layer only: the
-- worker/timer that calls ClaimDue is stage 4b. Every statement is a CAS or
-- a lease write so two concurrent schedulers can never double-fire an
-- occurrence (SCHED-1/SCHED-2).

-- name: InsertWakeSchedule :one
INSERT INTO wake_schedules (
    id, owner_session_id, kind, message, next_run_at,
    every_ms, max_runs, until_at, state, occurrence, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, 'active', 0, ?, ?
)
RETURNING *;

-- name: GetWakeSchedule :one
SELECT * FROM wake_schedules WHERE id = ?;

-- name: ListWakeSchedulesForOwner :many
SELECT * FROM wake_schedules WHERE owner_session_id = ? ORDER BY next_run_at ASC, id ASC;

-- name: CountActiveWakeSchedulesForOwner :one
SELECT COUNT(*) FROM wake_schedules WHERE owner_session_id = ? AND state = 'active';

-- name: CancelWakeSchedule :execrows
-- Owner-checked, idempotent CAS: only an active row moves to cancelled. A
-- done/cancelled row is a no-op (SCHED-5); a foreign owner never matches.
UPDATE wake_schedules
SET state = 'cancelled', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE id = ? AND owner_session_id = ? AND state = 'active';

-- name: CancelAllWakeSchedulesForOwner :execrows
-- `sessions reset` (R8A-3 sibling): no active schedule may survive a wiped
-- history. Done/cancelled rows are history and stay.
UPDATE wake_schedules
SET state = 'cancelled', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE owner_session_id = ? AND state = 'active';

-- name: ListDueWakeSchedules :many
-- The due, claimable rows, soonest first, bounded by the caller's limit.
-- Read INSIDE the claim transaction: the writer connection holds the write
-- lock from BEGIN, so nothing can lease them out before the CAS below.
SELECT * FROM wake_schedules
WHERE state = 'active' AND next_run_at <= ?
  AND (lease_owner IS NULL OR lease_expires_at <= ?)
ORDER BY next_run_at ASC, id ASC
LIMIT ?;

-- name: ClaimWakeScheduleLease :execrows
-- Per-row CAS half of the atomic claim: 0 rows means another leader won
-- this row between the read and the write; the caller skips it.
UPDATE wake_schedules
SET lease_owner = ?, lease_expires_at = ?, updated_at = ?
WHERE id = ? AND state = 'active'
  AND (lease_owner IS NULL OR lease_expires_at <= ?);

-- name: CompleteOnceWakeSchedule :execrows
-- once: the occurrence fired -> done. Keyed by the lease so a stale fire
-- (lease lost or recovered) is a no-op; the row can never fire twice
-- (SCHED-1).
UPDATE wake_schedules
SET state = 'done', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state = 'active';

-- name: FinishLoopWakeSchedule :execrows
-- loop: max_runs reached or until_at crossed -> done instead of advancing.
-- Same lease-keyed CAS as CompleteOnceWakeSchedule.
UPDATE wake_schedules
SET state = 'done', lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state = 'active';

-- name: AdvanceLoopWakeOccurrence :execrows
-- loop: occurrence advances and next_run_at moves to the FIRST schedule
-- boundary strictly after the fired one that is >= the fire time (computed
-- by the caller from the SCHEDULED time, never from the fire time -- SCHED-3).
UPDATE wake_schedules
SET occurrence = occurrence + 1, next_run_at = ?, lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE id = ? AND lease_owner = ? AND state = 'active';

-- name: RecoverExpiredWakeLeases :execrows
-- Lease recovery: a claimed row whose lease expired without firing goes
-- back to unclaimed. The row was never fired (firing clears the lease in
-- the same tx as the advance), so no duplicate is possible (SCHED-2).
UPDATE wake_schedules
SET lease_owner = NULL, lease_expires_at = NULL, updated_at = ?
WHERE state = 'active' AND lease_owner IS NOT NULL AND lease_expires_at < ?;

-- name: NextDueWakeScheduleAt :one
-- Earliest next_run_at among active rows, for the scheduler's timer.
SELECT MIN(next_run_at) AS next_due_at FROM wake_schedules WHERE state = 'active';
