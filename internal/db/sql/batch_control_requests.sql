-- name: CreateBatchControlRequest :one
-- The FK binds the request to the node's owner, host and root incarnation.
INSERT INTO batch_control_requests (
    request_id, owner_session_id, root_id, node_id, host_id, created_at
) VALUES (@request_id, @owner_session_id, @root_id, @node_id, @host_id, @created_at)
RETURNING *;

-- name: GetBatchControlRequest :one
SELECT * FROM batch_control_requests
WHERE request_id = @request_id AND root_id = @root_id AND host_id = @host_id
  AND owner_session_id = @owner_session_id;

-- name: ListPendingBatchControlRequests :many
SELECT * FROM batch_control_requests
WHERE host_id = @host_id AND root_id = @root_id AND owner_session_id = @owner_session_id
  AND status = 'pending'
ORDER BY created_at, request_id;

-- name: AnswerBatchControlRequest :execrows
-- Compose with the owning loop's projection; an answer is not termination.
UPDATE batch_control_requests SET status = 'answered', answer_code = @answer_code,
    answer_sequence = @answer_sequence, answered_at = @answered_at
WHERE request_id = @request_id AND root_id = @root_id AND node_id = @node_id
  AND host_id = @host_id AND owner_session_id = @owner_session_id AND status = 'pending';
