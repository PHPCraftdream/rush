-- name: InsertSessionNotice :one
-- session_notices carries notices with no async_jobs row (supervision,
-- wake_failed marker, background SDK shell completion, wake_only timeout --
-- doc sec.2/3.2). Created with delivery='pending' so it is a drain
-- candidate immediately.
INSERT INTO session_notices (
    owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, 'pending', ?, ?, ?
)
RETURNING *;

-- name: GetSessionNotice :one
SELECT * FROM session_notices WHERE id = ?;

-- name: ListPendingSessionNoticesForOwner :many
-- Drain candidates (doc sec.3.3), oldest first so history order matches
-- occurrence order.
SELECT * FROM session_notices WHERE owner = ? AND delivery = 'pending' ORDER BY id ASC;

-- name: PullPendingSessionNotice :one
-- The pull UPDATE...RETURNING (doc sec.3.3), same shape as
-- PullPendingAsyncJobNotice: one transaction per notice, caller INSERTs the
-- history message and stores notice_message_id in the same tx. 0 rows
-- affected means another leader already won this row.
UPDATE session_notices
SET delivery = 'done', updated_at = ?
WHERE id = ? AND delivery = 'pending'
RETURNING *;

-- name: SetSessionNoticeMessageID :execrows
UPDATE session_notices SET notice_message_id = ?, updated_at = ?
WHERE id = ?;

-- name: VoidPendingSessionNotice :execrows
-- A pulled notice whose "task still running" condition failed (doc sec.3.4:
-- supervision notice is debt only while the scope still has a running row;
-- wake_only timeout notice voids if the task is no longer running) becomes
-- void instead of done.
UPDATE session_notices SET delivery = 'void', updated_at = ?
WHERE id = ? AND delivery = 'pending';

-- name: MarkSessionNoticesReactedForOwner :execrows
-- Same reaction rule as MarkAsyncJobsReactedForOwner, for the notices table.
UPDATE session_notices SET reacted = 1, updated_at = ?
WHERE owner = ? AND wake = 1 AND reacted = 0 AND delivery = 'done';

-- name: SetSessionNoticesWakeZeroPendingForOwners :execrows
-- Stop transitivity (DUR-9, doc sec.3.8), notices half of
-- SetAsyncJobsWakeZeroPendingForOwners.
UPDATE session_notices SET wake = 0, updated_at = ?
WHERE owner IN (sqlc.slice('owner_ids')) AND delivery IN ('pending', 'done') AND wake = 1;

-- name: ListSessionNoticesForOwner :many
-- Reader for `sessions jobs`/`sessions why`.
SELECT * FROM session_notices WHERE owner = ? ORDER BY id ASC;

-- name: PurgeSessionNoticesOlderThan :execrows
-- Same retention pass as PurgeAsyncJobsOlderThan (doc sec.3.7).
DELETE FROM session_notices WHERE delivery IN ('done', 'void') AND updated_at < ?;

-- name: CountSessionNoticesOlderThan :one
SELECT COUNT(*) FROM session_notices WHERE delivery IN ('done', 'void') AND updated_at < ?;
