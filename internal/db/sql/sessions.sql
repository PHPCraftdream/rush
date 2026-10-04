-- name: CreateSession :one
-- The workspace_root/git_branch values are supplied by the session service
-- (createWithOrigin, #1142 step C): the Go layer is the single fill point, so
-- every creation path (Create*, task, title, fork) binds the row to the
-- workspace of the process that created it.
INSERT INTO sessions (
    id,
    parent_session_id,
    title,
    message_count,
    prompt_tokens,
    completion_tokens,
    cost,
    summary_message_id,
    updated_at,
    created_at,
    smart_model_provider,
    smart_model_id,
    fast_model_provider,
    fast_model_id,
    yolo_enabled,
    origin,
    cost_parent_id,
    workspace_root,
    git_branch
) VALUES (
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    ?,
    null,
    strftime('%s', 'now'),
    strftime('%s', 'now'),
    ?,
    ?,
    ?,
    ?,
    0,
    ?,
    ?,
    ?,
    ?
) RETURNING *;

-- GetLastSession lives in run_cost_queries.go (hand-written): sqlc's SQLite
-- parser cannot compile the two-column recursive CTE that carries the root
-- ancestor through the recursive member.

-- name: UpdateSessionModels :exec
-- Partial update: a NULL arg for a slot's provider/id pair leaves that slot
-- untouched (COALESCE falls back to the current column value); a non-NULL
-- arg (including an explicit empty string) overwrites it. This lets callers
-- distinguish "don't touch this slot" from "clear this slot back to
-- inheriting the folder/system default" (smart_model_id = '' is the existing
-- "no override" convention the app layer already reads via != "").
UPDATE sessions
SET
    smart_model_provider = COALESCE(sqlc.narg('smart_model_provider'), smart_model_provider),
    smart_model_id = COALESCE(sqlc.narg('smart_model_id'), smart_model_id),
    fast_model_provider = COALESCE(sqlc.narg('fast_model_provider'), fast_model_provider),
    fast_model_id = COALESCE(sqlc.narg('fast_model_id'), fast_model_id),
    updated_at = strftime('%s', 'now')
WHERE id = sqlc.arg('id');

-- name: UpdateSessionWorkerReviewerModels :exec
-- Same partial-update semantics as UpdateSessionModels: a NULL arg leaves
-- that slot untouched, a non-NULL arg (including an explicit empty string)
-- overwrites it.
UPDATE sessions
SET
    worker_model_provider = COALESCE(sqlc.narg('worker_model_provider'), worker_model_provider),
    worker_model_id = COALESCE(sqlc.narg('worker_model_id'), worker_model_id),
    reviewer_model_provider = COALESCE(sqlc.narg('reviewer_model_provider'), reviewer_model_provider),
    reviewer_model_id = COALESCE(sqlc.narg('reviewer_model_id'), reviewer_model_id),
    updated_at = strftime('%s', 'now')
WHERE id = sqlc.arg('id');

-- name: UpdateSessionWorkerReviewerReasoningEffort :exec
UPDATE sessions
SET
    worker_model_reasoning_effort = ?,
    reviewer_model_reasoning_effort = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: GetSessionByID :one
SELECT *
FROM sessions
WHERE id = ? LIMIT 1;

-- name: ListSessions :many
SELECT *
FROM sessions
WHERE parent_session_id is NULL
ORDER BY updated_at DESC;

-- name: ListAllSessions :many
-- Returns every session including children (no parent_session_id filter).
-- Used by sessions gc to enumerate all sessions for garbage collection.
SELECT *
FROM sessions
ORDER BY updated_at DESC;

-- name: ListSubSessions :many
-- Returns every session whose parent_session_id matches the argument,
-- ordered oldest-first so callers reconstructing a fan-out get the
-- sub-agent results in dispatch order.
SELECT *
FROM sessions
WHERE parent_session_id = ?
ORDER BY created_at ASC;

-- name: IncrementSessionCost :one
-- Atomic additive update for the node's OWN cost. Safe under fan-out and
-- across processes. `cost_self` is the monotonic per-node ledger the
-- delegation-tree queries read; `cost` is a mirror kept for old binaries
-- (rollback path, #1130). Returns the updated row so the caller can
-- refresh its snapshot.
UPDATE sessions
SET
    cost = cost + ?,
    cost_self = cost_self + ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?
RETURNING *;

-- name: IncrementSessionCostIfUnderMax :execrows
-- task #782 (K-2, P1 release blocker): plain IncrementSessionCost has no
-- budget predicate, so two concurrent callers (a real turn and a cache
-- keep-alive replay, or two replays) can both read cost < maxCost, then
-- both call IncrementSessionCost, and jointly overshoot maxCost (e.g.
-- 0.09 -> 0.14 against a 0.10 cap). Moving the budget check into the WHERE
-- clause of the additive UPDATE itself closes that TOCTOU window: only ONE
-- of two racing callers can win when their combined delta would cross max,
-- because SQLite serializes writers and the second writer's WHERE
-- re-evaluates cost as already updated by the first.
--
-- This is a NEW query, not a modified IncrementSessionCost: that query has
-- other callers (agent_title.go, agent_turn.go, agent_compaction.go,
-- coordinator cost transfer, sessions_reset) which do not carry a maxCost
-- budget in the same shape and must keep their existing unconditional
-- semantics.
--
-- Returns rows affected: 0 means the charge was refused because the
-- subtree budget + delta would meet or exceed max_cost. The subtree
-- predicate lives in Go (session service, BEGIN IMMEDIATE tx): sqlc's
-- parser cannot bind a WITH to an UPDATE, and the budget must be read in
-- the same write transaction (SQLite serializes writers) to keep the #782
-- TOCTOU closed. This query is the unconditional arm of that method.
UPDATE sessions
SET
    cost = cost + sqlc.arg('delta'),
    cost_self = cost_self + sqlc.arg('delta'),
    updated_at = strftime('%s', 'now')
WHERE id = sqlc.arg('id');

-- name: RenameSession :exec
UPDATE sessions
SET
    title = ?
WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions
WHERE id = ?;

-- name: UpdateSessionSystemPrompt :exec
UPDATE sessions
SET
    system_prompt = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: UpdateSessionReasoningEffort :exec
UPDATE sessions
SET
    smart_model_reasoning_effort = ?,
    fast_model_reasoning_effort = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: GetSessionCostAccounting :one
SELECT cost, parent_cost_accounted
FROM sessions
WHERE id = ? LIMIT 1;

-- name: SetParentCostAccounted :exec
UPDATE sessions
SET
    parent_cost_accounted = ?,
    updated_at = strftime('%s', 'now')
WHERE id = ?;

-- name: GetSubtreeSpent :one
-- The run-cost reading of one node (#1130): spent = SUM(cost_self) over the
-- node's delegation subtree. Budget = max(spent - cost_base(node), 0),
-- clamped by the reader; cost_base comes from GetSessionCostBase (sqlc's
-- editor cannot expand two parameters across a CTE + correlated subquery).
-- The UNION makes a corrupted cost_parent_id cycle terminate and counts
-- every node once.
WITH RECURSIVE sub(node) AS (
    SELECT s0.id FROM sessions s0 WHERE s0.id = ?
    UNION
    SELECT s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
SELECT CAST(COALESCE(SUM(n.cost_self), 0) AS REAL) AS spent FROM sub JOIN sessions n ON n.id = sub.node;

-- name: GetSessionCostBase :one
SELECT cost_base FROM sessions WHERE id = ?;

-- name: GetSubtreeUpdatedAt :one
-- Activity of one node's delegation subtree (max updated_at): what
-- `--continue`, `sessions list` ordering and `sessions cost --since` read so
-- a busy child keeps its root visible WITHOUT writing the parent's
-- updated_at from the child (#1130).
WITH RECURSIVE sub(node) AS (
    SELECT s0.id FROM sessions s0 WHERE s0.id = ?
    UNION
    SELECT s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
SELECT CAST(COALESCE(MAX(n.updated_at), 0) AS INTEGER) AS updated_at FROM sub JOIN sessions n ON n.id = sub.node;

-- name: ListSessionEndReasonsForIDs :many
-- Batched ended_reason read for arbitrary ids (top-level AND child
-- sessions, which the session list does not carry). A missing row is not an
-- error: a deleted session simply has no end fact.
SELECT id, ended_reason FROM sessions WHERE id IN (sqlc.slice('session_ids'));
