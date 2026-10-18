-- +goose Up
-- +goose StatementBegin
-- Durable wake schedules (stage 4a, docs/plans/2026-09-24-agent-wakes-and-
-- async-job-control.md sec.4, contract 2026-09-27-wake-tools-contract.md
-- sec.1/sec.6). One row per schedule ("wake_" + uuid id). A schedule is
-- owned by its creating session; the FK cascades like async_jobs'
-- owner_session_id, so a deleted session takes its schedules with it (the
-- operator decision for `sessions reset` is CANCEL, not delete -- the reset
-- path cancels active rows explicitly, done/cancelled history stays).
--
-- All timestamps are UTC unix seconds. next_run_at is the SCHEDULED time,
-- never "now + every": a loop's next occurrence advances from the missed
-- scheduled time, so idle gaps are never caught up (at most one immediate
-- firing after downtime -- SCHED-3).
CREATE TABLE IF NOT EXISTS wake_schedules (
    id               TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN ('once', 'loop')),
    message          TEXT NOT NULL,
    next_run_at      INTEGER NOT NULL,
    every_ms         INTEGER NOT NULL DEFAULT 0, -- loop only; 0 for once
    max_runs         INTEGER,                    -- loop only; NULL = unbounded (until_at still applies)
    until_at         INTEGER,                    -- loop only; NULL = none
    state            TEXT NOT NULL CHECK (state IN ('active', 'done', 'cancelled')),
    occurrence       INTEGER NOT NULL DEFAULT 0, -- completed occurrences so far
    lease_owner      TEXT,                       -- ClaimDue holder; NULL = unclaimed
    lease_expires_at INTEGER,                    -- UTC unix seconds; expired lease is free again
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- The scheduler's claim scan (ClaimDue) and NextDue's earliest-due read.
CREATE INDEX IF NOT EXISTS idx_wake_schedules_due ON wake_schedules (state, next_run_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_wake_schedules_due;
DROP TABLE IF EXISTS wake_schedules;
-- +goose StatementEnd
