-- +goose Up
-- Durable external-driver marker (docs/reviews/2026-09-29-async-phase4-round1-
-- rerun-design.md, Problem 2): which `rush run` loop drives a session. The
-- driver is alive exactly when its host is alive (DUR-5: the host's OS lock),
-- so no clock is ever stored or read for liveness -- claimed_at and pid are
-- display only. Any other process never starts a reaction turn for a session
-- whose row names a live host.
--
-- host_id is an async_hosts.id with NO REFERENCES clause on purpose (same as
-- async_jobs.host_id): the host row may be reaped independently and a dead
-- host must never block a takeover write. The session FK cascades, so a
-- deleted session takes its marker with it.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS session_drivers (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    host_id    TEXT NOT NULL,
    pid        INTEGER NOT NULL,
    claimed_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_drivers_host_id ON session_drivers (host_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_session_drivers_host_id;
DROP TABLE IF EXISTS session_drivers;
-- +goose StatementEnd
