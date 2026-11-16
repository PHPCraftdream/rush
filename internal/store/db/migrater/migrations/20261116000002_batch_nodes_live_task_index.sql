-- +goose Up
-- The wake_only check-in scanner (batch_timeout_checkin.go) pages live task
-- nodes in (created_at, node_id) order every 10s under a 1s budget. Without an
-- index it scanned every batch_nodes row, parsing each task_payload as JSON,
-- and sorted the result: the scan grows with the store's history, not with the
-- handful of live tasks, and missed its budget on busy machines. This partial
-- index holds only live task nodes, already in keyset order.
CREATE INDEX idx_batch_nodes_live_task ON batch_nodes (created_at, node_id)
    WHERE kind = 'task' AND state IN ('pending','starting','running','cancelling');

-- +goose Down
DROP INDEX IF EXISTS idx_batch_nodes_live_task;
