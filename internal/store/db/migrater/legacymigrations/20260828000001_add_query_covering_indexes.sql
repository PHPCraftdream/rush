-- Close the query-plan gaps found by profiling `rush run` startup and DB
-- read paths on a real dev DB (178 sessions, 15400 messages). EXPLAIN QUERY
-- PLAN on the pre-migration schema showed:
--
--   ListMessagesBySession           SEARCH ... idx_messages_session_id
--                                    USE TEMP B-TREE FOR ORDER BY
--   ListMessagesBySessionPaginated  (same)
--   ListUserMessagesBySession       (same)
--   ListSessions                    SEARCH ... idx_sessions_parent_session_id
--                                    USE TEMP B-TREE FOR ORDER BY
--   ListSubSessions                 (same)
--   GetLastSession                  SCAN sessions
--                                    USE TEMP B-TREE FOR ORDER BY
--
-- Each of these filters on one column but sorts on another, so SQLite could
-- seek the filter via the existing single-column index but still had to
-- spool the matching rows into a temporary b-tree to satisfy ORDER BY.
-- GetLastSession is the worst case: no WHERE clause at all, so it fell back
-- to a full table scan just to find the single most-recently-updated
-- session (rush run --continue's lookup, run on every invocation).
--
-- The composite indexes below put the sort column right after the filter
-- column, so SQLite's own row order inside each index already matches the
-- query's ORDER BY -- the row producer becomes a plain index range scan with
-- no separate sort step. (SQLite's non-unique b-tree indexes always end with
-- rowid as an implicit final key, which is exactly the tiebreaker
-- ListMessagesBySession/ListUserMessagesBySession/ListMessagesBySessionPaginated
-- already ask for via `rowid` in their ORDER BY, so a two-column
-- (session_id, created_at) index is sufficient -- no need to name rowid in
-- the index definition.)
--
-- idx_messages_session_id, idx_sessions_parent_session_id, and
-- idx_files_session_id are dropped because the new composite indexes below
-- are strict supersets for every query that used them: any lookup that only
-- needs the filter column (not the sort) is served just as well by a
-- composite index's leading column. Keeping both would mean every INSERT/
-- UPDATE on these tables maintains two indexes for a filter pattern one
-- already covers, for no read-side benefit.

-- +goose Up
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_session_id;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_messages_session_id_created_at
ON messages (session_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_parent_session_id;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_sessions_parent_session_id_updated_at
ON sessions (parent_session_id, updated_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_sessions_parent_session_id_created_at
ON sessions (parent_session_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at
ON sessions (updated_at);
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_files_session_id;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_files_session_id_created_at
ON files (session_id, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_files_session_id_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_files_session_id ON files (session_id);
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_updated_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_parent_session_id_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_parent_session_id_updated_at;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_sessions_parent_session_id ON sessions (parent_session_id);
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_session_id_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages (session_id);
-- +goose StatementEnd
