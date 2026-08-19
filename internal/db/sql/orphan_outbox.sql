-- Task #340's original claim/mark-done/mark-failed/release-for-retry model
-- (ClaimOrphanOutboxEntry, MarkOrphanOutboxEntryDone, MarkOrphanOutboxEntryFailed,
-- ReleaseOrphanOutboxEntryForRetry, CleanupOldDoneOrphanOutboxEntries) was
-- superseded by task #426's atomic DrainOrphanOutboxEntry (single
-- insert-to-main-queue + delete-from-outbox transaction, no intermediate
-- 'processing'/'done'/'failed' state to get stuck in). Removed as dead
-- code -- task #440 follow-up decision: a genuinely malformed,
-- never-enqueueable entry now retries every drain tick forever instead of
-- reaching a terminal 'failed' state. Accepted deliberately rather than
-- reintroducing attempts-tracking into the atomic transaction: the FK
-- ON DELETE CASCADE on session_id already closes the realistic failure
-- mode (session deleted -> row cascades away on its own); what's left is
-- an operationally-visible (slog.Error per tick), not silent, edge case
-- for data that was malformed from the start.
--
-- That last part was revisited (2026-08-18 release-readiness review): "forever"
-- is unbounded log and DB churn every 15s for a row that can never succeed.
-- RecordOrphanOutboxFailure below reinstates `attempts`/`max_attempts`/the
-- 'failed' terminal state WITHOUT reintroducing the claim model: it is a
-- separate, single UPDATE on the failure path only, never part of the atomic
-- drain transaction, and it takes no ownership of the row. Nothing waits on
-- it, nothing recovers it, and a crash between the failed drain and this
-- write simply means the attempt was not counted.
--
-- RecordOrphanOutboxFailure notes:
--
--   * It is a separate UPDATE on the failure path only, never part of the
--     atomic drain transaction, and takes no ownership of the row. Nothing
--     waits on it and nothing recovers it.
--   * Two pump instances can both count the same failed attempt, so a poison
--     row may quarantine after fewer than max_attempts real ticks. That is
--     the correct direction to be wrong in -- sooner, never later -- and
--     avoiding it would need exactly the per-instance claim state this
--     design removed.
--   * Scoped to status = 'pending', so an already-quarantined row cannot be
--     re-counted and this can never resurrect a terminal row.
--
-- KEEP EVERY FILE UNDER internal/db/sql/ PURE ASCII. sqlc v1.30.0 miscounts
-- query spans when a comment contains a multi-byte character: it appears to
-- measure offsets in bytes and positions in runes, so each non-ASCII
-- character shifts the generated SQL's end by the difference. Adding a
-- comment with em-dashes here silently truncated this query's tail
-- (RETURNING ... became RETURNING ... max_atte -- a statement that would
-- have failed at Prepare time), and a longer block corrupted an unrelated
-- query at the top of the file into invalid SQL. Every other .sql file in
-- this package is ASCII-only, which is why nothing had hit it before.
-- Reproduced and bisected directly; see the task filed against the bug.

-- name: WriteToOrphanOutbox :one
-- Write a call to the orphan outbox when main run queue enqueue fails.
-- Returns the outbox row (or error on write failure).
INSERT INTO orphan_call_outbox (
    id,
    session_id,
    call_data,
    status,
    attempts,
    max_attempts,
    created_at,
    updated_at
) VALUES (?, ?, ?, 'pending', 0, 5, ?, ?)
RETURNING *;

-- name: ListPendingOrphanOutboxEntries :many
-- Get all pending outbox entries for recovery (scanned by pump).
SELECT id, session_id, call_data, status, attempts, max_attempts, last_error, created_at, updated_at
FROM orphan_call_outbox
WHERE status = 'pending'
ORDER BY created_at ASC;

-- name: GetOrphanOutboxEntry :one
-- Get a single entry by ID.
SELECT id, session_id, call_data, status, attempts, max_attempts, last_error, created_at, updated_at
FROM orphan_call_outbox
WHERE id = ?;

-- name: DeleteOrphanOutboxEntryIfPending :execrows
-- Delete an orphan outbox entry only if it's still pending (for atomic drain).
-- Returns the number of rows deleted (0 or 1).
-- Used by DrainOrphanOutboxEntry to atomically move entries to the main queue.
DELETE FROM orphan_call_outbox
WHERE id = ? AND status = 'pending';

-- name: RecordOrphanOutboxFailure :one
-- Count one failed drain attempt and quarantine the row at max_attempts.
-- See this file's header for why this is a separate write, why a double
-- count across pump instances is acceptable, and why RETURNING is `*`.
UPDATE orphan_call_outbox
SET attempts = attempts + 1,
    last_error = ?,
    updated_at = ?,
    status = IIF(attempts + 1 >= max_attempts, 'failed', 'pending')
WHERE id = ? AND status = 'pending'
RETURNING *;
