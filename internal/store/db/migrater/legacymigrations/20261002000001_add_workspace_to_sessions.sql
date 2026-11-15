-- #1142 step C, docs/plans/2026-10-01-shared-data-dir.md sec.1 (invariant
-- WS-1): a session belongs to ONE workspace, and only the owning process may
-- run it. Additive only: both sessions columns and the queue_tasks column
-- default to '' so pre-migration rows (and old binaries, which INSERT without
-- the new columns) keep working; no backfill is needed (see the design doc).

-- +goose Up
ALTER TABLE sessions ADD COLUMN workspace_root TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN git_branch TEXT NOT NULL DEFAULT '';
-- The `--continue` / fold-model ranking (GetLastSession) only ever considers
-- TOP-LEVEL rows, so the key is partial: a busy child session never pollutes
-- the ordering of its root.
CREATE INDEX idx_sessions_workspace_updated ON sessions (workspace_root, updated_at) WHERE parent_session_id IS NULL;
-- One queue task queue per checkout (P1 of the design doc): the runner's
-- claim/reclaim filters by this column, so a shared data dir does not hand a
-- linked worktree's tasks to the main checkout's runner.
ALTER TABLE queue_tasks ADD COLUMN workspace_root TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE queue_tasks DROP COLUMN workspace_root;
DROP INDEX IF EXISTS idx_sessions_workspace_updated;
ALTER TABLE sessions DROP COLUMN git_branch;
ALTER TABLE sessions DROP COLUMN workspace_root;
