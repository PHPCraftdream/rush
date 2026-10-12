-- name: ReserveBatchNode :one
-- Reserve every accepted node in one caller-owned transaction before dispatch.
INSERT INTO batch_nodes (
    node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
    tool_call_id, input_hash, origin_cli, mode, on_fail, notify, max_parallel,
    task_kind, task_group, task_payload, timeout_ns, child_session_id,
    state, cause, token, attempts, control_state, control_error, control_pending,
    outcome_state, outcome_code, outcome_summary, response_envelope_ref,
    root_sequence, projection_sequence, created_at, updated_at
) VALUES (
    @node_id, @root_id, @owner_session_id, @host_id, @parent_id, @ordinal, @kind,
    @tool_call_id, @input_hash, @origin_cli, @mode, @on_fail, @notify, @max_parallel,
    @task_kind, @task_group, @task_payload, @timeout_ns, @child_session_id,
    @state, @cause, @token, @attempts, @control_state, @control_error, @control_pending,
    @outcome_state, @outcome_code, @outcome_summary, @response_envelope_ref,
    @root_sequence, @projection_sequence, @created_at, @updated_at
) RETURNING *;

-- name: GetBatchRoot :one
SELECT * FROM batch_nodes
WHERE root_id = @root_id AND node_id = root_id AND owner_session_id = @owner_session_id;

-- name: GetBatchNode :one
SELECT * FROM batch_nodes
WHERE root_id = @root_id AND node_id = @node_id AND owner_session_id = @owner_session_id;

-- name: ListBatchNodes :many
SELECT * FROM batch_nodes
WHERE root_id = @root_id AND owner_session_id = @owner_session_id
ORDER BY node_id;

-- name: ListBatchChildren :many
SELECT * FROM batch_nodes
WHERE root_id = @root_id AND parent_id = @parent_id AND owner_session_id = @owner_session_id
ORDER BY ordinal;

-- name: GetLiveBatchRoot :one
-- Hash comparison belongs to the caller; mismatches must not start new work.
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner_session_id AND tool_call_id = @tool_call_id
  AND node_id = root_id AND state IN ('pending', 'starting', 'running', 'cancelling');

-- name: ListLiveBatchRootsForOwner :many
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner_session_id AND node_id = root_id
  AND state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY created_at, root_id;

-- name: ListLiveBatchRootsForHost :many
SELECT * FROM batch_nodes
WHERE host_id = @host_id AND node_id = root_id
  AND state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY created_at, root_id;

-- name: AdvanceBatchRootSequence :execrows
-- Engine sequences can skip for a root; advance atomically with all projections.
UPDATE batch_nodes SET root_sequence = @sequence, updated_at = @updated_at
WHERE root_id = @root_id AND node_id = root_id AND host_id = @host_id
  AND owner_session_id = @owner_session_id AND root_sequence < @sequence;

-- name: ProjectBatchNode :execrows
-- Projects the loop's accepted view, not a competing terminal-state decision.
UPDATE batch_nodes SET
    state = @state, cause = @cause, token = @token, attempts = @attempts,
    control_state = @control_state, control_error = @control_error,
    control_pending = @control_pending, outcome_state = @outcome_state,
    outcome_code = @outcome_code, outcome_summary = @outcome_summary,
    response_envelope_ref = @response_envelope_ref, child_session_id = @child_session_id,
    projection_sequence = @sequence, updated_at = @updated_at
WHERE root_id = @root_id AND node_id = @node_id AND host_id = @host_id
  AND owner_session_id = @owner_session_id AND projection_sequence < @sequence;

-- name: SelectBatchNodeNotice :execrows
-- Same transaction as settlement; announcement/history is deferred to R8.
UPDATE batch_nodes SET
    notice_selected = 1, notice_kind = @notice_kind, notice_state = @notice_state,
    notice_cause = @notice_cause, notice_sequence = @notice_sequence,
    notice_ordinal = @notice_ordinal, notice_pending = @notice_pending,
    notice_active = @notice_active, notice_completed = @notice_completed,
    notice_failed = @notice_failed, notice_cancelled = @notice_cancelled,
    notice_timed_out = @notice_timed_out, notice_interrupted = @notice_interrupted,
    notice_skipped = @notice_skipped, delivery = 'pending', wake = @wake,
    updated_at = @updated_at
WHERE root_id = @root_id AND node_id = @node_id AND host_id = @host_id
  AND owner_session_id = @owner_session_id AND projection_sequence = @notice_sequence
  AND notice_selected = 0
  AND state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped');

-- name: DeleteBatchSubtree :execrows
-- Same-incarnation parent/root FKs cascade descendants and controls.
DELETE FROM batch_nodes
WHERE root_id = @root_id AND node_id = @node_id AND host_id = @host_id
  AND owner_session_id = @owner_session_id;
