-- name: BatchLiveNodesForOwners :many
-- Return all nodes of nonterminal roots, including settled delegation links.
SELECT n.* FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE r.owner_session_id IN (sqlc.slice('owners'))
  AND r.state IN ('pending', 'starting', 'running', 'cancelling')
ORDER BY r.created_at, r.root_id, n.ordinal, n.node_id;

-- name: BatchLevelNodes :many
-- One statement observes the root and its entire incarnation consistently.
SELECT * FROM batch_nodes
WHERE owner_session_id = @owner AND root_id = @root_id
ORDER BY ordinal, node_id;
