-- name: BatchOutboxPending :many
SELECT n.* FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = @owner AND n.notice_selected = 1
  AND n.delivery = 'pending' AND r.announced = 1
ORDER BY r.created_at, n.root_id, n.notice_sequence, n.notice_ordinal, n.node_id;

-- name: BatchOutboxPull :one
-- Recheck the root gate and oldest outstanding notice under the writer lock.
UPDATE batch_nodes SET delivery = 'done', updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.root_id = @root_id AND batch_nodes.node_id = @node_id
  AND batch_nodes.notice_selected = 1 AND batch_nodes.delivery = 'pending'
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.announced = 1)
  AND NOT EXISTS (SELECT 1 FROM batch_nodes earlier
    WHERE earlier.root_id = batch_nodes.root_id AND earlier.notice_selected = 1 AND earlier.delivery = 'pending'
      AND (earlier.notice_sequence < batch_nodes.notice_sequence
        OR (earlier.notice_sequence = batch_nodes.notice_sequence AND earlier.notice_ordinal < batch_nodes.notice_ordinal)
        OR (earlier.notice_sequence = batch_nodes.notice_sequence AND earlier.notice_ordinal = batch_nodes.notice_ordinal AND earlier.node_id < batch_nodes.node_id)))
RETURNING *;

-- name: BatchOutboxNameMessage :execrows
UPDATE batch_nodes SET notice_message_id = @message_id, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.root_id = @root_id AND batch_nodes.node_id = @node_id
  AND notice_selected = 1 AND delivery = 'done' AND notice_message_id IS NULL;

-- name: BatchOutboxAnnounce :execrows
-- Ack election is independent of execution state; both fields change together.
UPDATE batch_nodes SET announced = 1, announce_message_id = @message_id, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.root_id = @root_id AND batch_nodes.node_id = batch_nodes.root_id AND batch_nodes.announced = 0;

-- name: BatchOutboxSetAnnouncementMessage :execrows
UPDATE batch_nodes SET announce_message_id = @message_id, updated_at = @updated_at
WHERE owner_session_id = @owner AND root_id = @root_id AND node_id = root_id AND announced = 1;

-- name: BatchOutboxInlineElect :one
-- Only an unannounced one-task root can elect its result as completion.
UPDATE batch_nodes SET delivery = 'done', wake = 0, reacted = 1, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.root_id = @root_id AND batch_nodes.node_id = batch_nodes.root_id
  AND batch_nodes.announced = 0 AND batch_nodes.notice_selected = 1 AND batch_nodes.delivery = 'pending'
  AND batch_nodes.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped')
  AND (SELECT count(*) FROM batch_nodes c WHERE c.root_id = batch_nodes.root_id AND c.node_id != c.root_id) = 1
  AND EXISTS (SELECT 1 FROM batch_nodes c WHERE c.parent_id = batch_nodes.root_id AND c.kind = 'task')
RETURNING *;

-- name: BatchOutboxDebtNodes :many
SELECT n.* FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = @owner AND n.notice_selected = 1 AND r.announced = 1
  AND n.wake = 1 AND n.reacted = 0 AND n.delivery IN ('pending', 'done')
ORDER BY n.root_id, n.notice_sequence, n.notice_ordinal, n.node_id;

-- name: BatchOutboxDebtNotices :many
SELECT * FROM session_notices WHERE owner = @owner AND wake = 1 AND reacted = 0
  AND delivery IN ('pending', 'done') ORDER BY id;

-- name: BatchOutboxReactNodes :execrows
UPDATE batch_nodes SET reacted = 1, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND notice_selected = 1 AND batch_nodes.delivery = 'done' AND batch_nodes.wake = 1 AND batch_nodes.reacted = 0
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.announced = 1);

-- name: BatchOutboxClearFailure :execrows
UPDATE batch_nodes SET reacted_failed = 0, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND reacted_failed = 1;

-- name: BatchOutboxAccountNode :execrows
-- Shared snapshot guard for attempt increments and failure closure.
UPDATE batch_nodes SET wake_attempts = batch_nodes.wake_attempts + @increment,
  reacted = @reacted, reacted_failed = @failed, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND batch_nodes.root_id = @root_id AND batch_nodes.node_id = @node_id
  AND batch_nodes.notice_message_id = @message_id AND batch_nodes.notice_selected = 1
  AND batch_nodes.delivery = 'done' AND batch_nodes.wake = 1 AND batch_nodes.reacted = 0
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.announced = 1);

-- name: BatchOutboxAccountNotice :execrows
UPDATE session_notices SET wake_attempts = wake_attempts + @increment,
  reacted = @reacted, reacted_failed = @failed, updated_at = @updated_at
WHERE owner = @owner AND id = @id AND notice_message_id = @message_id
  AND delivery = 'done' AND wake = 1 AND reacted = 0;

-- name: BatchOutboxFailedNodes :many
SELECT * FROM batch_nodes WHERE batch_nodes.owner_session_id = @owner AND notice_selected = 1 AND reacted_failed = 1
ORDER BY root_id, notice_sequence, notice_ordinal, node_id;

-- name: BatchOutboxWakeZero :execrows
UPDATE batch_nodes SET wake = 0, updated_at = @updated_at
WHERE batch_nodes.owner_session_id = @owner AND notice_selected = 1 AND delivery IN ('pending', 'done') AND wake = 1;
