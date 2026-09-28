-- name: RegisterAsyncHost :one
-- Lazy registration at first claim (doc sec.3.6): the host row is
-- display-only bookkeeping, created once per process lifetime alongside the
-- OS lock file. Never consulted to decide liveness.
INSERT INTO async_hosts (id, pid, label, started_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetAsyncHost :one
SELECT * FROM async_hosts WHERE id = ?;

-- name: ListAsyncHosts :many
-- Reader for `sessions jobs`/`sessions hosts` display.
SELECT * FROM async_hosts ORDER BY started_at ASC;

-- name: ListDistinctRunningHostIDs :many
-- Every host_id that currently owns a 'running' row -- the candidate set a
-- recovery sweep probes (doc sec.3.6/3.7). Liveness itself is decided by
-- the host lock module (OS lock probe), not by this query.
SELECT DISTINCT host_id FROM async_jobs WHERE state = 'running';

-- name: DeleteAsyncHostIfNoJobs :execrows
-- Owner deletes its own row at exit if it has no rows (doc sec.3.6); a
-- recoverer holding a dead host's lock calls this too, after deleting the
-- lock file itself. No FK enforces this -- the guard is explicit here.
-- The subquery is correlated against async_hosts.id (not a second bound
-- parameter): sqlc's SQLite plugin does not reliably rewrite a repeated
-- same-named placeholder once one occurrence sits in the outer WHERE and
-- the other inside a subquery's WHERE (confirmed against sqlc v1.30.0 --
-- bare `?`, repeated @id, and two distinctly-named args all either
-- under-counted the bind values or left an occurrence unrewritten,
-- producing a query that fails at runtime). Correlating on the outer
-- table's own column needs only ONE real parameter and sidesteps the bug
-- entirely.
DELETE FROM async_hosts
WHERE id = ? AND NOT EXISTS (SELECT 1 FROM async_jobs WHERE host_id = async_hosts.id);

-- name: ListAsyncHostsWithNoJobs :many
-- Retention candidates (doc sec.3.7): host rows with zero referencing
-- async_jobs rows of ANY state. Caller still must verify the lock file
-- itself is gone/dead before treating a host as reapable.
SELECT * FROM async_hosts
WHERE id NOT IN (SELECT DISTINCT host_id FROM async_jobs);

-- name: ClaimAsyncJob :one
-- Durable idempotent start. ON CONFLICT DO NOTHING mirrors
-- EnqueueRunQueueEntry: a caller retrying the same (owner_session_id,
-- tool_call_id) after a crash must not error just because an earlier
-- attempt already committed the row -- it reads the existing row instead
-- (GetAsyncJob), same as before.
--
-- child_session_id is claimed here directly for delegations (doc sec.3.8,
-- reversing the rejected branch's FK-driven "claim empty, fill in later"
-- two-step): the id is deterministic and child_session_id carries no FK,
-- so there is nothing blocking writing it at claim time. Callers pass NULL
-- for plain (kind='command') jobs.
INSERT INTO async_jobs (
    owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id,
    origin_cli, state, host_id, announced, delivery, wake, reacted,
    deadline_at, timeout_kind, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, 0, 'none', 0, 0, ?, ?, ?, ?
)
ON CONFLICT (owner_session_id, tool_call_id) DO NOTHING
RETURNING *;

-- name: GetAsyncJob :one
SELECT * FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: GetRunningAsyncJobByChildSession :one
-- ASYNC-01 conflict check inside the claim transaction (doc sec.3.8): a
-- second delegation naming the same child session while a RUNNING row
-- already claims it must fail (dead-host row: recover it in the same
-- attempt instead) rather than silently queuing behind it.
SELECT * FROM async_jobs WHERE child_session_id = ? AND state = 'running';

-- name: MarkAsyncJobAnnounced :execrows
-- Ack gate (DUR-7): "started" and announced=1 are one transaction with NO
-- condition on state -- a job that raced to terminal before its own
-- "started" tool-result committed must still be marked announced (the
-- caller checks rows-affected==0 only to detect a since-deleted row, e.g.
-- a Rerun that removed it out from under this transaction).
UPDATE async_jobs SET announced = 1, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: DeleteUnannouncedAsyncJob :execrows
-- Abort: the "started" tool-result write itself failed, so nothing durable
-- should remain (ASYNC-05). Scoped to announced=0 so a row that won the
-- ack-gate race concurrently is never deleted out from under it.
DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0;

-- name: TransitionAsyncJobTerminalPreserveVoid :one
-- The ONE terminal-transition CAS (DUR-1/DUR-2/step-2 review): a terminal
-- state, its cause (notice_kind), the result payload, delivery, and wake
-- are all set by this single statement, scoped to state='running' so only
-- the first committer wins -- every other concurrent caller sees 0 rows
-- affected and must re-read the row (GetAsyncJob) and accept whatever
-- state is there instead of retrying the same transition. This is the
-- ONLY terminal-transition query (doc sec.3.8 is explicit the terminal
-- transition always preserves void, so the earlier non-preserving sibling
-- this replaced -- TransitionAsyncJobTerminal -- is gone, not a second,
-- parallel path): a row already voided by Rerun truncation must not be
-- resurrected to 'pending' by a late-arriving terminal transition (e.g.
-- job_kill racing the history truncation).
--
-- delivery is a PARAMETER, not a hardcoded 'pending' literal (step 3): every
-- cause passes 'pending' today; step 6 passes 'done' for job_kill once its
-- own tool response carries the real output (doc sec.3.2), so that row never
-- also surfaces as a history notice via the pull.
UPDATE async_jobs
SET state = ?, notice_kind = ?, result_summary = ?, result_is_error = ?,
    delivery = CASE delivery WHEN 'void' THEN 'void' ELSE ? END,
    wake = ?, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND state = 'running'
RETURNING *;

-- name: ListPendingAsyncJobNoticesForOwner :many
-- Candidates for the drain's pull (doc sec.3.3): announced=1 is required --
-- an unannounced job never produces a notice (DUR-7).
SELECT * FROM async_jobs
WHERE owner_session_id = ? AND delivery = 'pending' AND announced = 1
ORDER BY created_at ASC;

-- name: PullPendingAsyncJobNotice :one
-- The pull UPDATE...RETURNING (doc sec.3.3): one transaction per notice --
-- caller does this, then INSERTs the history message, then stores
-- notice_message_id via SetAsyncJobNoticeMessageID, all in the SAME tx. 0
-- rows affected (checked by the caller via RETURNING yielding sql.ErrNoRows)
-- means another leader already won this row; the caller must roll back
-- rather than proceed to insert a duplicate message.
UPDATE async_jobs
SET delivery = 'done', updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND delivery = 'pending' AND announced = 1
RETURNING *;

-- name: SetAsyncJobNoticeMessageID :execrows
-- Second half of the pull transaction: records where the notice landed.
UPDATE async_jobs SET notice_message_id = ?, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: VoidPendingAsyncJobNotice :execrows
-- A pulled notice whose task-still-running condition failed (doc sec.3.4,
-- supervision/wake_only void-at-drain rule) becomes void instead of done.
UPDATE async_jobs SET delivery = 'void', updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND delivery = 'pending';

-- name: AsyncReactionDebtExists :one
-- Doc sec.3.4: one indexed EXISTS, backed by the partial indexes
-- idx_async_jobs_debt and idx_session_notices_debt "(owner) WHERE wake=1
-- AND reacted=0 AND delivery<>'void'" on both tables. The async_jobs branch
-- additionally requires announced=1 (doc: "task rows with announced=1" --
-- review fix): a job that reaches terminal before its own "started"
-- tool-result commits is wake=1/delivery=pending/reacted=0/announced=0; an
-- unannounced row can never produce a notice (DUR-7) and the drain pull
-- (ListPendingAsyncJobNoticesForOwner/PullPendingAsyncJobNotice) already
-- skips it, so without this guard the debt check falsely reports debt,
-- driving a wasted provider turn with nothing to react to -- and if the ack
-- then aborts, the row is deleted (DeleteUnannouncedAsyncJob) and the turn
-- was wasted for nothing. session_notices carries no announced concept
-- (doc sec.3.2: it has no ack gate), so its branch is unchanged.
SELECT
    EXISTS (
        SELECT 1 FROM async_jobs
        WHERE owner_session_id = @owner AND wake = 1 AND reacted = 0 AND delivery != 'void' AND announced = 1
    )
    OR EXISTS (
        SELECT 1 FROM session_notices
        WHERE owner = @owner AND wake = 1 AND reacted = 0 AND delivery != 'void'
    ) AS has_debt;

-- name: MarkAsyncJobsReactedForOwner :execrows
-- Reaction is recorded where it happens (doc sec.3.4): the same transaction
-- that persists a model step's real-content finish marks every wake=1,
-- reacted=0, delivery='done' row of this owner reacted=1. Not derived from
-- created_at/clock order -- every currently-done row qualifies, including
-- ones a compaction later strips from context.
UPDATE async_jobs SET reacted = 1, updated_at = ?
WHERE owner_session_id = ? AND wake = 1 AND reacted = 0 AND delivery = 'done';

-- name: IncrementAsyncJobWakeAttempts :execrows
-- Settle-by-failure step 1 (doc sec.3.4: "a temporary failure ... increments
-- the attempt counter in the row"): a temporary provider failure after a
-- wake-up call increments wake_attempts on the SPECIFIC rows the failed
-- turn was meant to react to, not every debt row of the owner (the doc is
-- explicit the closing/counting scope is fixed at the start of that turn,
-- not re-evaluated against whatever is pending now). Guarded by
-- wake=1 AND reacted=0 so a row that settled (by a real step, or by an
-- earlier failure closure) in the meantime is left alone.
UPDATE async_jobs SET wake_attempts = wake_attempts + 1, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id IN (sqlc.slice('tool_call_ids')) AND wake = 1 AND reacted = 0;

-- name: SettleAsyncJobsReactedFailed :execrows
-- Settle-by-failure step 2, after K=3 (doc sec.3.4): closes debt on exactly
-- the id set captured at the start of the failed turn -- doc: "only for the
-- rows that were done at the moment it started" -- not every currently-pending
-- row of the owner (a notice that arrived mid-retry must get its own future
-- wake-up call, not be silently absorbed into this closure). reacted_failed
-- distinguishes this from an ordinary step-persisted reaction
-- (MarkAsyncJobsReactedForOwner): it is set ONLY here, never by a real
-- step, so a child session can tell its parent the delegation failed
-- instead of succeeded-with-no-output.
UPDATE async_jobs SET reacted = 1, reacted_failed = 1, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id IN (sqlc.slice('tool_call_ids')) AND wake = 1 AND reacted = 0;

-- name: ListReactedFailedAsyncJobsForOwner :many
-- Reader for the parent-notification path (doc sec.3.4: "passes the parent
-- the text of the unreacted notices") -- the rows settle-by-failure closed
-- for this owner, whose text a failed delegation's parent must still see.
SELECT * FROM async_jobs WHERE owner_session_id = ? AND reacted_failed = 1 ORDER BY created_at ASC;

-- name: SetAsyncJobsWakeZeroPendingForOwners :execrows
-- Stop transitivity (DUR-9, doc sec.3.8): every pending row of the stopped
-- tree loses its wake bit in the same pass, so a race between natural
-- completion and Stop can never grant a stopped delegation a turn.
UPDATE async_jobs SET wake = 0, updated_at = ?
WHERE owner_session_id IN (sqlc.slice('owner_ids')) AND delivery IN ('pending', 'done') AND wake = 1;

-- name: ListRunningAsyncJobsForHost :many
-- Recovery sweep input (doc sec.3.7): every RUNNING row owned by a
-- particular host_id, for a leader/recoverer that has independently
-- confirmed (via the host lock module) that host_id is dead.
SELECT * FROM async_jobs WHERE host_id = ? AND state = 'running' ORDER BY created_at ASC;

-- name: ListRunningAsyncJobsForOwners :many
-- Own-area recovery/scope evaluation (doc sec.3.5/3.7): the leader's owner
-- id set restricted to currently-running rows. That set comes from walking
-- delegation rows (child_session_id), NOT parent_session_id -- doc sec.3.5
-- is explicit: "descendants via parent_session_id are not walked". No
-- lease/heartbeat filter -- liveness is decided per-host by the caller via
-- the host lock module, not by this query.
SELECT * FROM async_jobs
WHERE owner_session_id IN (sqlc.slice('owner_ids')) AND state = 'running'
ORDER BY created_at ASC;

-- name: ListAsyncJobsForOwner :many
-- Reader for `sessions jobs`/`sessions why`.
SELECT * FROM async_jobs WHERE owner_session_id = ? ORDER BY created_at ASC;

-- name: PurgeAsyncJobsOlderThan :execrows
-- Bounded retention (doc sec.3.7): a terminal, delivered-or-voided row past
-- the cutoff is deleted outright -- no soft delete, same precedent as
-- `sessions gc`'s own row deletion.
DELETE FROM async_jobs
WHERE state != 'running' AND delivery IN ('done', 'void') AND updated_at < ?;

-- name: CountAsyncJobsOlderThan :one
-- Same predicate as PurgeAsyncJobsOlderThan, read-only, for
-- `sessions gc --dry-run`.
SELECT COUNT(*) FROM async_jobs
WHERE state != 'running' AND delivery IN ('done', 'void') AND updated_at < ?;
