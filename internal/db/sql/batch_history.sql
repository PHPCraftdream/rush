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
-- The delivery='done' guard keeps a retried rerun from resurrecting a void.
UPDATE batch_nodes SET delivery = 'pending', wake_attempts = 0,
    reacted_failed = 0, updated_at = @updated_at
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

-- name: BatchRerunVoidNodesOfVoidedRoots :execrows
-- Nodes of roots the rerun voided (announce or legacy arm): their notice
-- messages were deleted too, but unlike a surviving root's nodes they must
-- never re-pend into the fresh history. Runs AFTER the repend and only
-- inside voided roots, so a node whose root survived keeps its re-pend.
UPDATE batch_nodes SET delivery = 'void', wake = 0, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.notice_selected = 1
  AND batch_nodes.delivery != 'void'
  AND batch_nodes.notice_message_id IN (sqlc.slice('message_ids'))
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
