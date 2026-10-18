-- R12-A TEST DATA ONLY. This directory is NOT part of the embedded
-- migration set (it is not under internal/db/migrations and nothing
-- go:embeds it); it is consumed only by tests via os.DirFS. Its version
-- (20991231000001) sorts after every embedded migration so a goose provider
-- over this FS sees it as the single pending migration on a fully-migrated
-- database.

-- +goose Up
-- Transaction handling: the goose v3 SQLite provider wraps every statement
-- of one migration file in a single transaction by default (absence of
-- the no-transaction annotation), so the UPDATE below and the DROP commit or
-- roll back together; no explicit BEGIN/COMMIT is added.
--
-- Destructive cutover rehearsal: void only JOB-BOUND session_notices rows
-- (owner + job_tool_call_id pairs with an async_jobs row, per the
-- session_notices schema in 20260928000002), then drop async_jobs. Unrelated
-- notices (job_tool_call_id NULL or pointing at no job row) keep their
-- delivery/wake, and all message rows are untouched. Historical REACTED
-- notices (reacted = 1, already consumed by a turn) keep their delivery/wake
-- and updated_at too: the selected policy voids UNREACTED notices only.
-- +goose StatementBegin
UPDATE session_notices
SET delivery = 'void', wake = 0, updated_at = strftime('%s', 'now')
WHERE EXISTS (
    SELECT 1 FROM async_jobs
    WHERE async_jobs.owner_session_id = session_notices.owner
      AND async_jobs.tool_call_id = session_notices.job_tool_call_id
)
  AND session_notices.reacted = 0;
DROP TABLE async_jobs;
-- +goose StatementEnd

-- +goose Down
-- Restores structure only, never data: an exact copy of the FINAL
-- pre-cutover async_jobs schema (bg_shell kind, claim_id,
-- announce_message_id, all indexes), created EMPTY. Rows dropped by Up are
-- unrecoverable by design.
-- +goose StatementBegin
CREATE TABLE async_jobs (
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
