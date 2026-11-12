-- +goose Up
CREATE TABLE session_agent_finalization_continuations (
    row_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    logical_call_id TEXT NOT NULL CHECK (logical_call_id <> ''),
    payload_hash TEXT NOT NULL CHECK (payload_hash <> ''),
    identities TEXT NOT NULL,
    ancestry TEXT NOT NULL,
    lease_token TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    consumed INTEGER NOT NULL DEFAULT 0 CHECK (consumed IN (0, 1))
);

-- +goose Down
DROP TABLE session_agent_finalization_continuations;
