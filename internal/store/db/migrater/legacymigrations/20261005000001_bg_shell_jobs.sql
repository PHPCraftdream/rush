-- +goose Up
-- R-BG-1 (#1126, docs/plans/2026-10-01-bg-shell-ledger.md): a background shell
-- started OUTSIDE the async-wrapped branch (SDK-origin or Drain turns on
-- context.Background(): the sync branch of asyncTool) survives the tool
-- response with NO async_jobs row -- its completion was a session_notices
-- row (kind=bg_shell_done) written later by the shell's OnDone callback,
-- i.e. the "second truth" the ledger design removes. Every such shell now
-- claims a REAL row (kind=bg_shell, tool_call_id = shell id) whose terminal
-- transition commits the result and the wake bit in ONE transaction
-- (AsyncJobStore.Transition, DUR-1).
--
-- kind's CHECK cannot be extended with ALTER TABLE, so this is a table
-- rebuild: identical columns, one added kind value. tool_call_id already
-- carries the shell id for bg_shell rows, so no shell_id column is added
-- (deviation from design section 2.4, recorded in the task report).
-- +goose StatementBegin
CREATE TABLE async_jobs_new (
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    tool_call_id     TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('command', 'agent', 'fetch', 'bg_shell')),
    tool_name        TEXT NOT NULL DEFAULT '',
    timeout_seconds  INTEGER NOT NULL DEFAULT 0,
    input_hash       TEXT NOT NULL,
    child_session_id TEXT,
    origin_cli       INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK (
        state IN ('running', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')
    ),
    notice_kind       TEXT NOT NULL DEFAULT '',
    host_id           TEXT NOT NULL,
    announced         INTEGER NOT NULL DEFAULT 0,
    delivery          TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    wake              INTEGER NOT NULL DEFAULT 0,
    reacted           INTEGER NOT NULL DEFAULT 0,
    wake_attempts     INTEGER NOT NULL DEFAULT 0,
    reacted_failed    INTEGER NOT NULL DEFAULT 0,
    deadline_at       INTEGER,
    timeout_kind      TEXT,
    result_summary    TEXT,
    result_is_error   INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    claim_id          TEXT NOT NULL DEFAULT '',
    announce_message_id TEXT,
    PRIMARY KEY (owner_session_id, tool_call_id)
);
INSERT INTO async_jobs_new (
    owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash,
    child_session_id, origin_cli, state, notice_kind, host_id, announced,
    delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed,
    deadline_at, timeout_kind, result_summary, result_is_error, created_at,
    updated_at, claim_id, announce_message_id
) SELECT
    owner_session_id, tool_call_id, kind, tool_name, timeout_seconds, input_hash,
    child_session_id, origin_cli, state, notice_kind, host_id, announced,
    delivery, notice_message_id, wake, reacted, wake_attempts, reacted_failed,
    deadline_at, timeout_kind, result_summary, result_is_error, created_at,
    updated_at, claim_id, announce_message_id
FROM async_jobs;
DROP TABLE async_jobs;
ALTER TABLE async_jobs_new RENAME TO async_jobs;
CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
    ON async_jobs (child_session_id)
    WHERE state = 'running' AND child_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
    ON async_jobs (owner_session_id)
    WHERE wake = 1 AND reacted = 0 AND delivery != 'void' AND announced = 1;
CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
    ON async_jobs (state, updated_at)
    WHERE state != 'running';
-- +goose StatementEnd

-- +goose Down
-- A bg_shell row must not survive into a schema whose CHECK rejects the
-- kind: cancel every running one, drop the rest, then rebuild the table
-- with the original three-value CHECK.
-- +goose StatementBegin
UPDATE async_jobs SET state = 'cancelled', delivery = 'done', wake = 0, reacted = 1, updated_at = strftime('%s', 'now') WHERE kind = 'bg_shell' AND state = 'running';
DELETE FROM async_jobs WHERE kind = 'bg_shell';
CREATE TABLE async_jobs_old_schema (
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    tool_call_id     TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('command', 'agent', 'fetch')),
    tool_name        TEXT NOT NULL DEFAULT '',
    timeout_seconds  INTEGER NOT NULL DEFAULT 0,
    input_hash       TEXT NOT NULL,
    child_session_id TEXT,
    origin_cli       INTEGER NOT NULL DEFAULT 0,
    state            TEXT NOT NULL CHECK (
        state IN ('running', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')
    ),
    notice_kind       TEXT NOT NULL DEFAULT '',
    host_id           TEXT NOT NULL,
    announced         INTEGER NOT NULL DEFAULT 0,
    delivery          TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    wake              INTEGER NOT NULL DEFAULT 0,
    reacted           INTEGER NOT NULL DEFAULT 0,
    wake_attempts     INTEGER NOT NULL DEFAULT 0,
    reacted_failed    INTEGER NOT NULL DEFAULT 0,
    deadline_at       INTEGER,
    timeout_kind      TEXT,
    result_summary    TEXT,
    result_is_error   INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    claim_id          TEXT NOT NULL DEFAULT '',
    announce_message_id TEXT,
    PRIMARY KEY (owner_session_id, tool_call_id)
);
INSERT INTO async_jobs_old_schema SELECT * FROM async_jobs;
DROP TABLE async_jobs;
ALTER TABLE async_jobs_old_schema RENAME TO async_jobs;
CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
    ON async_jobs (child_session_id)
    WHERE state = 'running' AND child_session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
    ON async_jobs (owner_session_id)
    WHERE wake = 1 AND reacted = 0 AND delivery != 'void' AND announced = 1;
CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
    ON async_jobs (state, updated_at)
    WHERE state != 'running';
-- +goose StatementEnd
