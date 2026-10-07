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
-- Test/diagnostic reader: no production caller, no `sessions hosts` command.
-- `label` is the registrant's App label ("app" at both NewAsyncJobStore call
-- sites), display-only bookkeeping.
SELECT * FROM async_hosts ORDER BY started_at ASC;

-- name: ListDistinctRecoverableHostIDs :many
-- Every host_id that owns a row a dead host would leave behind -- the
-- candidate set a recovery sweep probes (doc sec.3.6/3.7): a 'running' row,
-- any unannounced row (a terminal announced=0 row is a leak of a host that
-- died before its "started" result committed, R2A-7), or a job_kill row that
-- is done but never got its result message (R2A-8). Liveness itself is
-- decided by the host lock module (OS lock probe), not by this query.
SELECT DISTINCT host_id FROM async_jobs
WHERE state = 'running'
   OR announced = 0
   OR (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL);

-- name: DeleteTerminalUnannouncedAsyncJobsForHost :execrows
-- ASYNC-05 for a dead host (R2A-7): a job that reached a terminal state
-- before its own "started" result committed (announced=0) never produces a
-- notice and would otherwise leak forever and block its tool_call_id --
-- recovery reads only state='running'. Deleted without a trace, like the
-- running unannounced rows of the same host.
DELETE FROM async_jobs WHERE host_id = ? AND announced = 0 AND state != 'running';

-- name: RependJobKillRowsWithoutNoticeForHost :execrows
-- DUR-11 for job_kill on a dead host (R2A-8): job_kill's transition commits
-- delivery='done', reacted=1 first and the result message (which names
-- notice_message_id) later; a host that died between left a 'done' row that
-- is never pulled and that Rerun cannot re-pend. Back to a plain pending,
-- wake=0 row (never debt, never a wake) so the next pull shows the result.
UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
WHERE host_id = ? AND state != 'running' AND delivery = 'done' AND reacted = 1 AND wake = 0
  AND notice_kind = 'job_kill' AND notice_message_id IS NULL;

-- name: RependJobKillRowWithoutNotice :execrows
-- Live-process twin of RependJobKillRowsWithoutNoticeForHost (R2A-8): the
-- job_kill tool call finished without its fused result write (an error
-- result, a cancelled context, a failed transaction), so the row this same
-- call had just marked done/reacted names no message. Keyed by the CLAIM the
-- call's own transition won, not by tool_call_id (R3A-2): a later claim under
-- a reused id archives the killed row to another tool_call_id while job_kill
-- is still running, and a claim id names exactly one incarnation of a job.
UPDATE async_jobs SET delivery = 'pending', reacted = 0, wake = 0, reacted_failed = 0, wake_attempts = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND claim_id = @claim_id AND claim_id != ''
  AND state != 'running' AND delivery = 'done' AND reacted = 1 AND wake = 0
  AND notice_kind = 'job_kill' AND notice_message_id IS NULL;

-- name: DeleteAsyncHostIfNoJobs :execrows
-- Deletes a host row that no async_jobs row references (doc sec.3.6): the owner
-- at exit, a recoverer or the empty-host reaper for a dead host. Every caller
-- deletes the ROW before the lock file, so a file can outlive its row until
-- whoever wins its exclusive lock removes it (else purgeOrphanHostLockFiles).
-- No FK enforces this -- the guard is explicit here.
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
--
-- claim_id (A11, migration 20260929000002): a random id minted once per
-- claim, carried by the executor for this claim's lifetime and threaded
-- into TransitionAsyncJobTerminalPreserveVoid's CAS -- closes the ABA where
-- a deleted-then-re-claimed row would otherwise let a stale executor's late
-- result commit onto the NEW claim just because both share
-- (owner_session_id, tool_call_id) and state='running'.
INSERT INTO async_jobs (
    owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash, child_session_id,
    origin_cli, state, host_id, claim_id, announced, delivery, wake, reacted,
    deadline_at, timeout_kind, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, 0, 'none', 0, 0, ?, ?, ?, ?
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
-- caller checks rows-affected==0 only to detect a since-deleted row: the
-- owner session was deleted, which cascades; Rerun never deletes rows).
UPDATE async_jobs SET announced = 1, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: SetAsyncJobAnnounceMessageID :execrows
-- Second half of the ack gate's fused transaction (AnnounceStarted): records
-- the "started" tool-result message that announced this row, so a Rerun can
-- void the row by the message it actually deleted instead of by
-- tool_call_id (which a provider may reuse per response). See migration
-- 20260929000003.
UPDATE async_jobs SET announce_message_id = ?, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ?;

-- name: DeleteUnannouncedAsyncJob :execrows
-- Abort: the "started" tool-result write itself failed, so nothing durable
-- should remain (ASYNC-05). Scoped to announced=0 so a row that won the
-- ack-gate race concurrently is never deleted out from under it; a row Stop
-- already cancelled is kept as the durable record of that Stop.
DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND announced = 0 AND state <> 'cancelled';

-- name: DeleteUnannouncedAsyncJobForClaim :execrows
-- Recovery's twin of DeleteUnannouncedAsyncJob (R3A-3): recoverers no longer
-- hold the dead host's exclusive lock, so two of them can list the same row;
-- one deletes it and a live host may claim the same tool_call_id before the
-- other's delete runs. Keyed by the listed row's claim so only that
-- incarnation can be removed.
DELETE FROM async_jobs WHERE owner_session_id = ? AND tool_call_id = ? AND claim_id = ? AND announced = 0;

-- name: TransitionAsyncJobTerminalPreserveVoid :one
-- The ONE terminal-transition CAS (DUR-1/DUR-2/step-2 review): a terminal
-- state, its cause (notice_kind), the result payload, delivery, wake, and
-- reacted are all set by this single statement, scoped to state='running'
-- so only the first committer wins -- every other concurrent caller sees 0
-- rows affected and must re-read the row (GetAsyncJob) and accept whatever
-- state is there instead of retrying the same transition. This is the
-- ONLY terminal-transition query (doc sec.3.8 is explicit the terminal
-- transition always preserves void, so the earlier non-preserving sibling
-- this replaced -- TransitionAsyncJobTerminal -- is gone, not a second,
-- parallel path): a row already voided by Rerun truncation must not be
-- resurrected to 'pending' by a late-arriving terminal transition (e.g.
-- job_kill racing the history truncation).
--
-- delivery is a PARAMETER, not a hardcoded 'pending' literal (step 3): every
-- cause passes 'pending' except step 6's job_kill, which passes 'done' once
-- its own tool response carries the real output (doc sec.3.2), so that row
-- never also surfaces as a history notice via the pull. reacted is also a
-- PARAMETER (step 6): every cause passes 0 (a row cannot have reacted
-- anything while still running) except job_kill, which passes 1 in the SAME
-- statement -- so a job_kill row is neither debt nor a future notice the
-- instant it commits, with no separate write.
--
-- claim_id (A11): the CALLER always supplies a concrete value -- either the
-- claim_id its own executor captured at Claim time (the real ABA guard), or
-- -- for a caller with no claim_id of its own (recovery, test seeding) --
-- the store's Go layer (AsyncJobStore.Transition) first reads the row's
-- CURRENT claim_id in the SAME transaction and passes that back in, so the
-- predicate below is a no-op for them (always matches whatever is already
-- there) and their pre-A11 "any running row matches" behavior is unchanged.
UPDATE async_jobs
SET state = ?, notice_kind = ?, result_summary = ?, result_is_error = ?,
    delivery = CASE delivery WHEN 'void' THEN 'void' ELSE ? END,
    wake = ?, reacted = ?, updated_at = ?
WHERE owner_session_id = ? AND tool_call_id = ? AND state = 'running' AND claim_id = ?
RETURNING *;

-- name: RependAsyncJobsByNoticeMessageIDs :execrows
-- Rerun undo-truncation (doc sec.3.8): a row announced BEFORE the
-- truncation point whose OWN notice landed in the deleted tail goes back to
-- pending/reacted=0 so the next turn on the new branch pulls it again
-- (ASYNC-04). Matched by notice_message_id, which is only ever set once a
-- row reaches delivery='done'; the `delivery = 'done'` guard (A4) makes that
-- explicit rather than implied -- without it, a retried Rerun could turn an
-- already-'void' row (delivery is not terminal until every writer says so)
-- back into 'pending'. wake_attempts/reacted_failed are reset (A1): a row
-- re-entering the pending pool starts its settle-by-failure counters fresh,
-- not with whatever an earlier, unrelated closure left behind. Must run
-- BEFORE the void queries in the same Rerun pass: a row whose OWN
-- tool call is ALSO in the deleted tail matches both queries, and void must
-- win for it.
UPDATE async_jobs SET delivery = 'pending', reacted = 0, reacted_failed = 0, wake_attempts = 0, updated_at = ?
WHERE owner_session_id = ? AND delivery = 'done' AND notice_message_id IN (sqlc.slice('message_ids'));

-- name: VoidAsyncJobsByAnnounceMessageIDs :many
-- Rerun truncation (doc sec.3.8): every row whose "started" tool-result
-- message (announce_message_id) is among the rows the truncation actually
-- deleted must never surface a notice, regardless of its current
-- delivery/state -- including a still-'running' row (its stop may have
-- raced or failed): the terminal-transition CAS always preserves an
-- existing 'void' (TransitionAsyncJobTerminalPreserveVoid), so writing void
-- here first closes that race for a late-arriving terminal transition too.
-- Keyed by the message id, not tool_call_id: a provider that numbers calls
-- per response reuses "call_0", so a tool_call_id match could void a KEPT
-- row (or miss an archived one). RETURNING hands the caller the rows to
-- stop once the transaction commits.
UPDATE async_jobs SET delivery = 'void', updated_at = @updated_at
WHERE owner_session_id = @owner AND announce_message_id IN (sqlc.slice('message_ids'))
RETURNING tool_call_id, state, child_session_id, host_id;

-- name: VoidAsyncJobsByToolCallIDs :many
-- Legacy arm of the Rerun void, for rows announced before migration
-- 20260929000003 (announce_message_id IS NULL): matched by the deleted
-- tail's tool_call_ids. Restricted to NULL so a row that DOES name its
-- announce message is never matched by a possibly-reused tool_call_id.
UPDATE async_jobs SET delivery = 'void', updated_at = ?
WHERE owner_session_id = ? AND announce_message_id IS NULL AND tool_call_id IN (sqlc.slice('tool_call_ids'))
RETURNING tool_call_id, state, child_session_id, host_id;

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

-- name: DeliverAsyncJobInline :execrows
-- A14 inline window (Tx2, docs/plans/2026-10-01-inline-window.md): the
-- inline tool-result message already carries the result, so the row's
-- delivery closes in the SAME transaction as that message's insert
-- (AnnounceInlineResult) -- exactly job_kill's DUR-11 shape. CAS on
-- claim_id (one incarnation of a job) and delivery='pending' only: a
-- running row can never be inline-delivered (the caller checked the
-- committed terminal state), and 0 rows means the row was voided by a
-- Rerun (or deleted) -- the announce half still commits, the caller falls
-- back to the ordinary in-memory tail. notice_kind is NOT touched: it
-- stays whatever the committed transition wrote, never a delivery marker.
UPDATE async_jobs
SET delivery = 'done', notice_message_id = ?, wake = 0, reacted = 1, updated_at = ?
WHERE owner_session_id = ? AND claim_id = ? AND claim_id != ''
  AND state != 'running' AND delivery = 'pending';

-- name: SetAsyncJobNoticeMessageIDForClaimIfDone :execrows
-- A3 (docs/reviews/2026-09-29-async-phase4-round1.md): job_kill's own
-- Transition call sets delivery='done' directly, bypassing the ordinary
-- pull (doc sec.3.2) -- so nothing else ever calls SetAsyncJobNoticeMessageID
-- for that row. This fuses job_kill's own tool-result message id onto it in
-- the SAME transaction as that message's insert (AnnounceJobKillResult),
-- satisfying the law "delivery='done' => the row names the message that
-- carries its result" (Rerun's RependAsyncJobsByNoticeMessageIDs, matched by
-- notice_message_id, could otherwise never find a job_kill'd tool call in a
-- deleted tail). Keyed by the killed row's CLAIM, not its tool_call_id
-- (R3A-2): a new claim under a reused id archives the killed row while
-- job_kill is still running, and a tool_call_id lookup would then name the
-- NEW row (or nothing). Guarded to delivery='done' AND notice_message_id IS
-- NULL, defensively: the caller passes a claim only after its own Transition
-- won, and no other writer re-pends or names a done job_kill row (Rerun
-- re-pends by notice_message_id, which an unnamed row never matches). The one
-- reachable 0-rows cause is deletion (the owner session's cascade); it is not
-- an error, the caller's tool-result message is persisted either way.
UPDATE async_jobs SET notice_message_id = @notice_message_id, updated_at = @updated_at
WHERE owner_session_id = @owner_session_id AND claim_id = @claim_id AND claim_id != ''
  AND delivery = 'done' AND notice_message_id IS NULL;

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

-- name: VisibleAsyncReactionDebtExists :one
-- Phase 4 step 4 review fix (P1): same shape as AsyncReactionDebtExists, scoped to
-- delivery='done' specifically instead of "!= 'void'" -- debt already
-- VISIBLE in history right now (the pull already succeeded for it), not
-- merely 'pending' (a pull that has not succeeded yet, or is permanently
-- failing). The Drain turn-start decision uses THIS query, not the plain
-- one: a permanently failing pull leaves rows stuck at 'pending' forever,
-- and reacting to 'pending' debt would force an endless chain of empty-
-- prompt provider turns with nothing new in history to react to. The
-- partial debt indexes (idx_async_jobs_debt/idx_session_notices_debt,
-- "WHERE wake=1 AND reacted=0 AND delivery != 'void'") do NOT serve this
-- query: SQLite does not infer "delivery != 'void'" from "delivery = 'done'".
-- async_jobs is narrowed by the (owner_session_id, tool_call_id) primary key
-- prefix, session_notices by idx_session_notices_owner (migration
-- 20260929000001).
SELECT
    EXISTS (
        SELECT 1 FROM async_jobs
        WHERE owner_session_id = @owner AND wake = 1 AND reacted = 0 AND delivery = 'done' AND announced = 1
    )
    OR EXISTS (
        SELECT 1 FROM session_notices
        WHERE owner = @owner AND wake = 1 AND reacted = 0 AND delivery = 'done'
    ) AS has_debt;

-- name: MarkAsyncJobsReactedForOwner :execrows
-- Reaction is recorded where it happens (doc sec.3.4): the same transaction
-- that persists a model step's real-content finish marks every wake=1,
-- reacted=0, delivery='done' row of this owner reacted=1. Not derived from
-- created_at/clock order -- every currently-done row qualifies, including
-- ones a compaction later strips from context.
UPDATE async_jobs SET reacted = 1, updated_at = ?
WHERE owner_session_id = ? AND wake = 1 AND reacted = 0 AND delivery = 'done';

-- name: GetAsyncJobByClaimID :one
-- Debt-snapshot row lookup (R2A-4): a snapshot names a row by its claim_id,
-- which survives the archive-on-reuse rename of tool_call_id -- a lookup by
-- tool_call_id text would find the NEW row a later claim put under the reused
-- id. claim_id '' (rows from before migration 20260929000002) has no
-- identity of its own, so those keep matching by tool_call_id too.
SELECT * FROM async_jobs
WHERE owner_session_id = @owner AND claim_id = @claim_id AND (claim_id <> '' OR tool_call_id = @tool_call_id);

-- name: IncrementAsyncJobWakeAttemptsForSnapshotRow :execrows
-- Settle-by-failure step 1 (doc sec.3.4: "a temporary failure ... increments
-- the attempt counter in the row") on ONE row of the failed turn's debt
-- snapshot (R2A-4/R2A-5). The row must still be the row the snapshot saw:
-- the same claim (claim_id, not tool_call_id text), still delivery='done'
-- and still carrying the notice message the snapshot recorded -- a Rerun
-- re-pend/re-pull changes notice_message_id, a void changes delivery -- and
-- still wake=1/reacted=0, so a row that settled or was re-pended in the
-- meantime is left alone.
UPDATE async_jobs SET wake_attempts = wake_attempts + 1, updated_at = @updated_at
WHERE owner_session_id = @owner AND claim_id = @claim_id AND (claim_id <> '' OR tool_call_id = @tool_call_id)
  AND delivery = 'done' AND COALESCE(notice_message_id, '') = CAST(@notice_message_id AS TEXT)
  AND wake = 1 AND reacted = 0;

-- name: SettleAsyncJobReactedFailedForSnapshotRow :execrows
-- Settle-by-failure step 2, after K=3 (doc sec.3.4): closes debt on ONE row of
-- the snapshot captured at the start of the failed turn -- doc: "only for the
-- rows that were done at the moment it started" -- under the same
-- still-the-row-the-snapshot-saw guard as
-- IncrementAsyncJobWakeAttemptsForSnapshotRow (R2A-4/R2A-5), so a late settle
-- (after the turn's release) can never touch a row a Rerun re-pended or
-- voided, or a later claim under a reused tool_call_id. reacted_failed
-- distinguishes this from an ordinary step-persisted reaction
-- (MarkAsyncJobsReactedForOwner): it is set ONLY here, never by a real
-- step, so a child session can tell its parent the delegation failed
-- instead of succeeded-with-no-output.
UPDATE async_jobs SET reacted = 1, reacted_failed = 1, updated_at = @updated_at
WHERE owner_session_id = @owner AND claim_id = @claim_id AND (claim_id <> '' OR tool_call_id = @tool_call_id)
  AND delivery = 'done' AND COALESCE(notice_message_id, '') = CAST(@notice_message_id AS TEXT)
  AND wake = 1 AND reacted = 0;

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
-- `sessions gc`'s own row deletion. `NOT (delivery='done' AND wake=1 AND
-- reacted=0)` (A5): a 'done' row with wake=1/reacted=0 IS unreacted debt --
-- already surfaced in history, just not yet reacted to -- and must survive
-- retention exactly like a 'running' row does, or a stuck/slow-to-react
-- owner silently loses its own obligation instead of ever settling it (by a
-- real turn or by settle-by-failure). Scoped to delivery='done' specifically,
-- matching the system's own debt definition everywhere else (idx_async_jobs_
-- debt, AsyncReactionDebtExists): a 'void' row is NEVER debt regardless of
-- its wake/reacted bits (Rerun's VoidAsyncJobsByToolCallIDs voids a row
-- without touching wake/reacted, so a voided row can carry stale wake=1/
-- reacted=0 from before it was voided -- that must not block its purge,
-- since a void row will never produce a notice to react to in the first
-- place).
-- R2A-10: a DELEGATION row (child_session_id set) is also kept while its
-- child session still has a running row or unreacted debt (async_jobs or
-- session_notices, pending included): isDurableDelegationChild recognises a
-- released delegation child only by this row, and a child whose row was
-- purged mid-work would get an uncapped Drain on the parent's agent.
-- R5A-1: a job_kill row that is 'done' with notice_message_id NULL (wake=0,
-- reacted=1: never debt) is the state dead-host recovery repairs
-- (RependJobKillRowsWithoutNoticeForHost, the third arm of
-- ListDistinctRecoverableHostIDs); purging it first would leave recovery
-- nothing to re-pend and the killed job's output would never reach the session.
DELETE FROM async_jobs
WHERE state != 'running' AND delivery IN ('done', 'void') AND updated_at < ?
  AND NOT (delivery = 'done' AND wake = 1 AND reacted = 0)
  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
  AND NOT (child_session_id IS NOT NULL AND (
        EXISTS (
            SELECT 1 FROM async_jobs c
            WHERE c.owner_session_id = async_jobs.child_session_id
              AND (c.state = 'running' OR (c.wake = 1 AND c.reacted = 0 AND c.delivery != 'void' AND c.announced = 1))
        )
        OR EXISTS (
            SELECT 1 FROM session_notices n
            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
        )
  ));

-- name: CountAsyncJobsOlderThan :one
-- Same predicate as PurgeAsyncJobsOlderThan, read-only, for
-- `sessions gc --dry-run`.
SELECT COUNT(*) FROM async_jobs
WHERE state != 'running' AND delivery IN ('done', 'void') AND updated_at < ?
  AND NOT (delivery = 'done' AND wake = 1 AND reacted = 0)
  AND NOT (delivery = 'done' AND notice_kind = 'job_kill' AND notice_message_id IS NULL)
  AND NOT (child_session_id IS NOT NULL AND (
        EXISTS (
            SELECT 1 FROM async_jobs c
            WHERE c.owner_session_id = async_jobs.child_session_id
              AND (c.state = 'running' OR (c.wake = 1 AND c.reacted = 0 AND c.delivery != 'void' AND c.announced = 1))
        )
        OR EXISTS (
            SELECT 1 FROM session_notices n
            WHERE n.owner = async_jobs.child_session_id AND n.wake = 1 AND n.reacted = 0 AND n.delivery != 'void'
        )
  ));

-- name: ClearReactedFailedForOwner :execrows
-- A1: a settle-by-failure closure ("this row's debt was closed by K=3
-- failed wake-up attempts, not a real reaction") is NOT a permanent,
-- unscoped fact about the owner -- it is superseded the moment the SAME
-- owner proves it is alive and answering again. Cleared from TWO call
-- sites: the real-reaction transaction (MarkReactedWithMessageUpdate, every
-- time a step finishes with real content) and claiming a fresh delegation
-- on a child session (Claim, before the new delegation's own rows can ever
-- be confused with the old one's). Without this, ListReactedFailedText/
-- refreshSubAgentCompletion read reacted_failed unscoped and report a
-- delegation "failed" forever after one transient closure, even once the
-- child session is back to answering normally (same delegation, a later
-- job) or has been handed a brand new delegation entirely.
UPDATE async_jobs SET reacted_failed = 0, updated_at = ?
WHERE owner_session_id = ? AND reacted_failed = 1;

-- name: ArchiveAsyncJobToolCallID :execrows
-- B14/A14b fix: a (owner_session_id, tool_call_id) key whose row is already
-- HISTORY (state != 'running' AND delivery IN ('done', 'void')) is renamed
-- out of the active key namespace so a REUSED tool_call_id (a provider that
-- numbers calls per response, e.g. "call_0") can claim a brand new row
-- immediately instead of being refused for up to 7 days as "already started
-- earlier"/"different input" (before phase 4 the id was freed at delivery;
-- phase 4's durable row otherwise outlives it). R2A-6: a terminal row that is
-- announced but not yet pulled (delivery='pending', announced=1) is archived
-- too -- its notice stays pullable by the owner under the archived text, and
-- it must not block the model's next call with the same id. An unannounced
-- terminal row is left alone: its "started" result is still to be written by
-- the caller that owns it. The archived row keeps its
-- own primary key column but under a new, collision-free text -- it stays
-- fully addressable by notice_message_id (Rerun's repend, readers) and by
-- every owner-scoped query; only a lookup BY THE ORIGINAL tool_call_id text
-- stops seeing it, which is exactly the point: that text is free again.
-- The `state != 'running'` guard is load-bearing, not redundant with the
-- delivery check: Rerun's VoidAsyncJobsByToolCallIDs can set delivery='void'
-- on a row that is STILL running (its stop may have raced or failed) --
-- renaming such a row's tool_call_id would orphan its own eventual
-- Transition call (which targets the OLD text) and, worse, risk that CAS
-- colliding with an unrelated NEW row later claimed under the freed text.
UPDATE async_jobs SET tool_call_id = @new_tool_call_id, updated_at = @updated_at
WHERE owner_session_id = @owner_session_id AND tool_call_id = @old_tool_call_id
  AND state != 'running'
  AND (delivery IN ('done', 'void') OR (delivery = 'pending' AND announced = 1));

-- name: VoidUndeliveredAsyncJobsForOwner :execrows
-- Full history wipe (`sessions reset`, R8A-3): every non-running row of the
-- owner still pending or already delivered-to-history becomes void, so the
-- next turn's pull cannot show it in the clean slate and no old
-- done-but-unreacted debt survives (debt excludes void). reacted_failed is
-- cleared with it: a settle-by-failure closure describes the wiped history.
-- Running rows are NOT touched: the caller refuses the reset while any
-- exists (their process is not the wiper's to stop).
UPDATE async_jobs SET delivery = 'void', reacted_failed = 0, updated_at = ?
WHERE owner_session_id = ? AND state != 'running' AND delivery IN ('pending', 'done');

-- name: AsyncJobDebtOwners :many
-- The async_jobs half of the batched reaction-debt read (architect decision
-- 13): the same predicates as AsyncReactionDebtExists' first branch, one
-- statement for a whole session list. sqlc.slice expands only its first
-- occurrence in a statement, so the two tables get one query each; the
-- store unions them (ReactionDebtOwners). Binding test vs the single-owner
-- query: TestReactionDebtOwners_MatchesSingleOwnerQuery.
SELECT DISTINCT owner_session_id AS owner FROM async_jobs
WHERE wake = 1 AND reacted = 0 AND delivery != 'void' AND announced = 1
  AND owner_session_id IN (sqlc.slice('owner_ids'));

-- name: SessionNoticeDebtOwners :many
-- The session_notices half (AsyncReactionDebtExists' second branch; the
-- table has no announced concept, doc sec.3.2).
SELECT DISTINCT owner FROM session_notices
WHERE wake = 1 AND reacted = 0 AND delivery != 'void'
  AND owner IN (sqlc.slice('owner_ids'));
