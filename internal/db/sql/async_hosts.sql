-- RETAINED host queries (package R12-D chunk 1): host-only bookkeeping,
-- job-independent. These are the batch-world host queries, re-pointed at
-- batch_nodes so the retained host lifecycle outlives the job table.
--
-- Naming: the *Row/*s suffixes mark "host row readers/writers" and keep the
-- generated method names distinct.

-- name: RegisterAsyncHostRow :one
-- Same lazy-registration shape as async_jobs.sql's RegisterAsyncHost (doc
-- sec.3.6): the host row is display-only bookkeeping, created once per
-- process lifetime alongside the OS lock file. Never consulted to decide
-- liveness.
INSERT INTO async_hosts (id, pid, label, started_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetAsyncHostRow :one
SELECT * FROM async_hosts WHERE id = ?;

-- name: ListAsyncHostRows :many
-- Test/diagnostic reader, job-independent twin of ListAsyncHosts.
SELECT * FROM async_hosts ORDER BY started_at ASC;

-- name: DeleteAsyncHostIfNoNodes :execrows
-- Host-row deletion guard, re-pointed from jobs to nodes. LEGACY semantics
-- (the deleted async_jobs host queries) deleted the host row only when
-- ZERO async_jobs rows of ANY state referenced it -- terminal/delivered rows
-- still counted, exactly like running ones, until a separate retention pass
-- purged them.
--
-- NEW semantics (requested R12-D change, the batch-world equivalent of "no
-- live nodes owned by the host"): the host row is deleted only when the host
-- owns ZERO LIVE batch_nodes rows (state in the live set
-- pending/starting/running/cancelling). Terminal node history alone does NOT
-- retain the host row -- a deliberate, requested semantic change from the
-- old all-states rule. The batch retention pass (DeleteBatchSubtree and
-- friends) is what eventually clears terminal history; after it removes the
-- last row, this guard can reap the host row. The live-set constants match
-- idx_batch_nodes_host_live's partial index WHERE clause.
--
-- The subquery is correlated against async_hosts.id (not a second bound
-- parameter), same sqlc-SQLite-plugin workaround as DeleteAsyncHostIfNoJobs:
-- correlating on the outer table's own column needs only ONE real parameter
-- and sidesteps the repeated-placeholder rewrite bug (confirmed against
-- sqlc v1.30.0, see the async_jobs.sql comment).
DELETE FROM async_hosts
WHERE id = ? AND NOT EXISTS (
    SELECT 1 FROM batch_nodes
    WHERE host_id = async_hosts.id
      AND state IN ('pending', 'starting', 'running', 'cancelling')
);

-- name: ListAsyncHostsWithNoNodes :many
-- Retention candidates, nodes-based twin of ListAsyncHostsWithNoJobs (doc
-- sec.3.7): host rows owning zero LIVE batch_nodes rows (same live-set rule
-- as DeleteAsyncHostIfNoNodes above). Caller still must verify the lock file
-- itself is gone/dead before treating a host as reapable.
SELECT * FROM async_hosts
WHERE id NOT IN (
    SELECT DISTINCT host_id FROM batch_nodes
    WHERE state IN ('pending', 'starting', 'running', 'cancelling')
);
