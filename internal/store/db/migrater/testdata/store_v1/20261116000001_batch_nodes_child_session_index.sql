-- +goose Up
-- Finalization ancestry reads look nodes up by the child session they spawned
-- (finalization_ancestors.go), once per ancestor, on the single writer
-- connection and again inside the commit transaction. Without this index each
-- lookup scans batch_nodes and its wide payload column, which held the writer
-- long enough to exhaust the 45s jobbatch commit budget (exit code 70).
CREATE INDEX idx_batch_nodes_child_session ON batch_nodes (child_session_id, node_id)
    WHERE child_session_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_batch_nodes_child_session;
