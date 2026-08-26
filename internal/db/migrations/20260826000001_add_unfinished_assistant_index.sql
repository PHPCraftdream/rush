-- Speed up startup recovery's orphan-assistant scan (task #774).
--
-- recoverInterruptedTurns used to call Sessions.ListAll then, for EVERY
-- session, Messages.List (full history) just to find the last message.
-- Measured on a real dev DB: 137 sessions, 10s+, tripping the sweep's own
-- 10s deadline. This partial index lets a new query (ListCandidateInterrupted-
-- AssistantSessions in messages.sql) find candidate sessions directly instead
-- of scanning every session's full message history.
--
-- The index covers exactly the base filter the new query ranks over
-- (role = 'assistant' AND finished_at IS NULL), ordered by (session_id,
-- created_at) so SQLite can serve the PARTITION BY session_id ORDER BY
-- created_at DESC window function from the index rather than a table scan.
-- SQLite auto-maintains partial indexes on every INSERT/UPDATE/DELETE, so
-- this is "read an index," not "run a smarter query that still walks every
-- row every time."

-- +goose Up
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_messages_unfinished_assistant
ON messages (session_id, created_at)
WHERE role = 'assistant' AND finished_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_messages_unfinished_assistant;
-- +goose StatementEnd
