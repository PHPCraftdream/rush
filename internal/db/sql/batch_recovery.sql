-- name: BatchRecoveryRoots :many
-- Include terminal ancestors with live descendants and terminal unannounced leaks.
SELECT r.* FROM batch_nodes r
WHERE r.node_id = r.root_id
  AND (r.announced = 0 OR EXISTS (
    SELECT 1 FROM batch_nodes n WHERE n.root_id = r.root_id
      AND n.state IN ('pending', 'starting', 'running', 'cancelling'))
    OR EXISTS (SELECT 1 FROM batch_control_requests c WHERE c.root_id = r.root_id AND c.status = 'pending'))
ORDER BY r.host_id, r.root_id;

-- name: BatchRecoveryElect :one
-- Obtain the writer BEFORE snapshot reads; independent stores serialize here.
UPDATE batch_nodes SET root_sequence = root_sequence
WHERE node_id = @root_id AND root_id = @root_id
  AND owner_session_id = @owner AND host_id = @host
RETURNING *;

-- name: BatchRecoveryInterrupt :execrows
-- Preserve latched causes, terminal outcomes and all existing outbox columns.
UPDATE batch_nodes SET state = 'interrupted', outcome_state = 'interrupted',
  outcome_code = 'host_lost', outcome_summary = 'Host lost; execution was not resumed',
  control_pending = 0, control_state = CASE WHEN control_pending = 1 THEN 'failed' ELSE control_state END,
  control_error = CASE WHEN control_pending = 1 THEN 'host_lost' ELSE control_error END,
  projection_sequence = @sequence, updated_at = @now
WHERE root_id = @root_id AND owner_session_id = @owner AND host_id = @host
  AND state IN ('pending', 'starting', 'running', 'cancelling');

-- name: BatchRecoveryNotice :execrows
-- No descendant cascade and no replacement of a preselected root notice.
UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'root',
  notice_state = state, notice_cause = cause, notice_sequence = @sequence,
  notice_ordinal = 0, notice_interrupted = (
    SELECT count(*) FROM batch_nodes n WHERE n.root_id = @root_id AND n.state = 'interrupted'),
  delivery = 'pending', wake = 0, updated_at = @now
WHERE batch_nodes.node_id = @root_id AND batch_nodes.root_id = @root_id
  AND batch_nodes.owner_session_id = @owner AND batch_nodes.host_id = @host AND batch_nodes.announced = 1
  AND batch_nodes.notice_selected = 0;

-- name: BatchRecoveryCloseControls :execrows
UPDATE batch_control_requests SET status = 'answered',
  answer_code = CASE WHEN EXISTS (SELECT 1 FROM batch_nodes n
    WHERE n.node_id = batch_control_requests.node_id AND n.outcome_code = 'host_lost')
    THEN 'host_lost' ELSE 'stale' END,
  answer_sequence = @sequence, answered_at = @now
WHERE batch_control_requests.root_id = @root_id AND batch_control_requests.owner_session_id = @owner AND batch_control_requests.host_id = @host
  AND batch_control_requests.status = 'pending';

-- name: BatchRetentionRoots :many
-- Count and purge both consume this exact whole-root predicate.
SELECT r.* FROM batch_nodes r
WHERE r.node_id = r.root_id AND r.updated_at < @cutoff
  AND NOT EXISTS (SELECT 1 FROM batch_control_requests c WHERE c.root_id = r.root_id AND c.status = 'pending')
  AND NOT EXISTS (SELECT 1 FROM batch_nodes n WHERE n.root_id = r.root_id
    AND (n.updated_at >= ?1 OR n.state IN ('pending', 'starting', 'running', 'cancelling')
      OR (n.notice_selected = 1 AND (n.delivery = 'pending'
        OR (n.delivery != 'void' AND n.wake = 1 AND n.reacted = 0)))))
  AND NOT EXISTS (SELECT 1 FROM batch_nodes delegation
    JOIN batch_nodes child ON child.owner_session_id = delegation.child_session_id
    WHERE delegation.root_id = r.root_id
      AND (child.state IN ('pending', 'starting', 'running', 'cancelling')
        OR (child.notice_selected = 1 AND (child.delivery = 'pending'
          OR (child.delivery != 'void' AND child.wake = 1 AND child.reacted = 0)))))
  AND NOT EXISTS (SELECT 1 FROM batch_nodes delegation
    JOIN session_notices child ON child.owner = delegation.child_session_id
    WHERE delegation.root_id = r.root_id AND child.delivery != 'void'
      AND (child.delivery = 'pending' OR (child.wake = 1 AND child.reacted = 0)))
ORDER BY r.root_id;

-- name: BatchRetentionWriter :exec
-- Acquire write ownership before evaluating retention; no history is deleted.
UPDATE batch_nodes SET root_sequence = root_sequence WHERE 0;
