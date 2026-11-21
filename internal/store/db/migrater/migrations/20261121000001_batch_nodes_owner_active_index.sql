-- +goose Up
-- The retention purge (BatchRetentionRoots, run at the start of every `rush run`
-- inside a write transaction on the single writer connection) joins
-- batch_nodes children by owner_session_id = delegation.child_session_id and
-- keeps only children that are live or still owe a notice. Every existing
-- owner_session_id index is partial on other terms, so none serves that join:
-- with wide rows the planner picks SCAN child per candidate root, and a store
-- with ~10k expired roots held the writer for over an hour (run queue pump and
-- batch check-ins starved behind it).
--
-- The index is partial on exactly the join's live-or-debt predicate (same
-- expression as the query, which is what lets the planner use it) so that it
-- stays tiny and is never a candidate for the hot owner-keyed statements: a
-- plain owner_session_id index there pulled them off the (root_id, ...) and
-- owner/root indexes onto a whole-owner range.
CREATE INDEX idx_batch_nodes_owner_active ON batch_nodes (owner_session_id)
    WHERE state IN ('pending', 'starting', 'running', 'cancelling')
        OR (notice_selected = 1 AND (delivery = 'pending'
            OR (delivery != 'void' AND wake = 1 AND reacted = 0)));

-- +goose Down
DROP INDEX IF EXISTS idx_batch_nodes_owner_active;
