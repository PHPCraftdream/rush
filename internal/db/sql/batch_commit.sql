-- Queries for the R11B CommitHook adapter and Runner incarnation fencing.
-- New queries only; shared node/control statements stay in their own files.

-- name: GetBatchRootIdentity :one
-- Resolve the owner of any projected root, including roots this Runner only
-- touches through admission; the caller fences host and incarnation itself.
SELECT owner_session_id, host_id, runner_id FROM batch_nodes
WHERE root_id = @root_id AND node_id = root_id;

-- name: UpdateBatchRunnerCheckpoint :execrows
-- Rootless lifecycle checkpoint for one (host, incarnation). Lower or equal
-- sequences are ignored; 0 is never written (Runner sequences start at 1).
INSERT INTO batch_runner_checkpoints (
    host_id, runner_id, sequence, event_type, final, created_at, updated_at
) VALUES (
    @host_id, @runner_id, @sequence, @event_type, @final, @updated_at, @updated_at
)
ON CONFLICT (host_id, runner_id) DO UPDATE SET
    sequence = excluded.sequence, event_type = excluded.event_type,
    final = excluded.final, updated_at = excluded.updated_at
WHERE excluded.sequence > batch_runner_checkpoints.sequence;
