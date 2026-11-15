-- #1130 R-LOOP-2: run cost as per-node columns read by delegation-tree
-- queries. Additive only: `cost` and `parent_cost_accounted` stay untouched
-- so an old binary (which silently ignores unknown goose versions) keeps
-- working on them and rollback is a DROP of the new columns.

-- +goose Up
ALTER TABLE sessions ADD COLUMN cost_self REAL NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cost_base REAL NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cost_parent_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_cost_parent ON sessions (cost_parent_id);

-- Delegation edges for pre-migration rows follow the agent-tool session id
-- scheme messageID + two dollar signs + toolCallID. Forks (parent_session_id
-- without that scheme) keep no cost edge: they are their own cost root.
UPDATE sessions SET cost_parent_id = parent_session_id
WHERE parent_session_id IS NOT NULL AND parent_session_id <> ''
  AND instr(id, char(36) || char(36)) > 0;

-- Backfill (design sec.3 of docs/plans/2026-10-01-run-cost.md). s(C) reads
-- the ORIGINAL `cost` column: a cost_self-based rewrite is forbidden.
-- s(C) = how much of child C was already transferred out (the watermark),
-- 0 when the row is inconsistent (cost below accounted).
-- in(X) = sum of s over the delegation children of X:
--   cost_self(X) = max(X.cost - in(X), 0)
UPDATE sessions SET
  cost_self = max(cost - COALESCE((
    SELECT SUM(CASE WHEN C.cost >= C.parent_cost_accounted THEN C.parent_cost_accounted ELSE 0 END)
    FROM sessions C WHERE C.cost_parent_id = sessions.id), 0), 0);

-- cost_base(X) = sum of e(Y) over the subtree of X, where
-- e(Y) = max(in(Y) - Y.cost, 0). The UNION makes a corrupted
-- cost_parent_id cycle terminate and counts every node once per root.
-- +goose StatementBegin
WITH RECURSIVE sub(root, node) AS (
  SELECT id, id FROM sessions
  UNION
  SELECT sub.root, s.id FROM sessions s JOIN sub ON s.cost_parent_id = sub.node
)
UPDATE sessions SET cost_base = COALESCE((
  SELECT SUM(max(COALESCE((
    SELECT SUM(CASE WHEN C.cost >= C.parent_cost_accounted THEN C.parent_cost_accounted ELSE 0 END)
    FROM sessions C WHERE C.cost_parent_id = sub.node), 0) - N.cost, 0))
  FROM sub JOIN sessions N ON N.id = sub.node
  WHERE sub.root = sessions.id
), 0)
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_cost_parent;
ALTER TABLE sessions DROP COLUMN cost_parent_id;
ALTER TABLE sessions DROP COLUMN cost_base;
ALTER TABLE sessions DROP COLUMN cost_self;
