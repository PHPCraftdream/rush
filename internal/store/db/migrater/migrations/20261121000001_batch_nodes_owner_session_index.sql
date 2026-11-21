-- +goose Up
-- The retention purge (BatchRetentionRoots, run at the start of every `rush run`
-- inside a write transaction on the single writer connection) joins
-- batch_nodes children by owner_session_id = delegation.child_session_id. Every
-- existing owner_session_id index is partial, so none serves that join: with
-- wide rows the planner picks SCAN child per candidate root, and a store with
-- ~10k expired roots held the writer for over an hour (run queue pump and
-- batch check-ins starved behind it). A plain index keeps the join a SEARCH
-- whatever shape the data has.
CREATE INDEX idx_batch_nodes_owner_session ON batch_nodes (owner_session_id);

-- +goose Down
DROP INDEX IF EXISTS idx_batch_nodes_owner_session;
