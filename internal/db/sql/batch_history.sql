-- name: BatchHistoryRootsForOwner :many
-- Activity snapshot input (R10c): every root incarnation of an owner,
-- terminal included, oldest first. Delivery/debt columns stay on the root
-- rows for the readers; node bodies come from BatchLevelNodes (batch_read.sql).
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner AND node_id = root_id
ORDER BY created_at, root_id;

-- name: BatchHistoryStateCounts :many
-- sessions audit/jobs counts over all incarnations of an owner, tasks only.
SELECT state, count(*) AS count FROM batch_nodes
WHERE owner_session_id = @owner AND kind = 'task'
GROUP BY state;

-- name: BatchResetDeleteOwnerNodes :execrows
-- Atomic reset (R10c): the refusal check lives in the same caller
-- transaction; once it passes, this removes every incarnation of the owner.
-- Self/parent root FKs and batch_control_requests cascade.
DELETE FROM batch_nodes WHERE owner_session_id = @owner_session_id;

-- name: BatchRerunRependNodesByNoticeMessageIDs :execrows
-- Selected-node delivery reconciliation: a done node whose own notice
-- message was deleted goes back to pending so the next turn pulls it again.
-- The dead message reference is cleared and the attempt counters reset
-- (mirrors BatchHistoryRependNodesByDeletedNoticeMessages), so the next
-- pull names a FRESH message and can never fail its CAS forever.
-- The delivery='done' guard keeps a retried rerun from resurrecting a void.
UPDATE batch_nodes SET delivery = 'pending', notice_message_id = NULL,
    wake_attempts = 0, reacted_failed = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND notice_selected = 1 AND delivery = 'done'
  AND notice_message_id IN (sqlc.slice('message_ids'));

-- name: BatchHistoryRependNodesByDeletedNoticeMessages :execrows
-- Delivery reconciliation (R10c, D-R10c-3): every selected node whose notice
-- message was deleted leaves 'done' and 'pending' as a fresh pending with the
-- dead message reference and attempt counters cleared, so the next pull
-- delivers against surviving history and debt accounting cannot key a deleted
-- message id. 'none' (never selected) and 'void' are untouched by design.
UPDATE batch_nodes SET delivery = 'pending', notice_message_id = NULL,
    wake_attempts = 0, reacted_failed = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND notice_selected = 1
  AND delivery IN ('done', 'pending')
  AND notice_message_id IN (sqlc.slice('message_ids'));

-- name: BatchRerunVoidNodesByNoticeMessageIDs :execrows
-- Void every selected node whose notice message was deleted; runs AFTER the
-- repend so a node matching both ends void. Void is written once: the
-- delivery != 'void' predicate keeps a second pass a 0-row no-op.
UPDATE batch_nodes SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND notice_selected = 1 AND delivery != 'void'
  AND notice_message_id IN (sqlc.slice('message_ids'));

-- name: BatchRerunVoidRootsByAnnounceMessageIDs :many
-- Roots whose "started" tool-result message was deleted are voided before
-- their pending/done notice can surface; still-nonterminal rows are returned
-- so the caller stops their executors only after this transaction commits.
UPDATE batch_nodes SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND node_id = root_id
  AND notice_selected = 1 AND delivery != 'void'
  AND announce_message_id IN (sqlc.slice('message_ids'))
RETURNING *;

-- name: BatchRerunVoidRootsByToolCallIDs :many
-- Legacy arm: roots with no announce message yet (never acked), matched by
-- the deleted tail's tool_call_id. The IS NULL guard means a root that named
-- its announce message is never matched by a provider-reused call id.
UPDATE batch_nodes SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE owner_session_id = @owner AND node_id = root_id
  AND notice_selected = 1 AND delivery != 'void' AND announce_message_id IS NULL
  AND tool_call_id IN (sqlc.slice('tool_call_ids'))
RETURNING *;

-- name: BatchRerunLiveRootsByAnnounceMessageIDs :many
-- D-R10c-live: the batch_nodes CHECK constraint forbids delivery='void' (and
-- wake=1) on a notice_selected=0 row, so an ordinary live root has NO selected
-- terminal notice and a void WRITE here would violate the schema. Pure
-- SELECT instead: still-nonterminal announced roots whose announce message
-- the rerun deleted are returned so the caller retires them via post-commit
-- stop routing (mirrors legacy TruncateForRerun, which voids running rows
-- regardless of state). Kept distinct by root_id from the void-write arm.
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner AND node_id = root_id
  AND state IN ('pending', 'starting', 'running', 'cancelling')
  AND announce_message_id IN (sqlc.slice('message_ids'));

-- name: BatchRerunLiveRootsByToolCallIDs :many
-- Legacy-arm counterpart: never-acked live roots (announce_message_id IS
-- NULL, the same I5 guard) matched by the deleted tail's tool_call_id,
-- returned for stop routing only -- no void write (see
-- BatchRerunLiveRootsByAnnounceMessageIDs).
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner AND node_id = root_id
  AND state IN ('pending', 'starting', 'running', 'cancelling')
  AND announce_message_id IS NULL
  AND tool_call_id IN (sqlc.slice('tool_call_ids'));

-- name: BatchRerunVoidNodesOfVoidedRoots :execrows
-- Nodes of roots the rerun voided (announce or legacy arm) end void -- ALL of
-- them, not only those whose message id matches the deleted set: the repend
-- above clears notice_message_id, so a message-id match would miss a
-- re-pended node of a voided root and leave an old-branch notice pending.
-- Void is written once: delivery != 'void' keeps a second pass a no-op. The
-- notice_selected = 1 guard keeps the write inside the schema CHECK (an
-- unselected row cannot take delivery='void').
UPDATE batch_nodes SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.notice_selected = 1
  AND batch_nodes.delivery != 'void'
  AND batch_nodes.root_id IN (SELECT v.root_id FROM batch_nodes AS v
                              WHERE v.owner_session_id = @owner
                                AND v.node_id = v.root_id AND v.delivery = 'void');

-- name: BatchListBoundNoticesByToolCallID :many
-- Legacy bound session_notices for one root's tool call, for disposal reads.
SELECT * FROM session_notices
WHERE owner = @owner AND job_tool_call_id = @tool_call_id
ORDER BY id;

-- name: BatchDisposeBoundNoticesByToolCallID :execrows
-- Dispose legacy bound session_notices superseded by a selected node notice:
-- void exactly once (delivery IN ('pending','done') makes a second call a
-- no-op) and drop the wake so the debt reader cannot count them again.
UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE owner = @owner AND job_tool_call_id = @tool_call_id
  AND delivery IN ('pending', 'done') AND wake = 1 AND reacted = 0;
