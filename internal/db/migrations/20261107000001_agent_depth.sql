-- +goose Up
CREATE TABLE session_agent_depth (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    remaining_depth INTEGER NOT NULL CHECK (remaining_depth BETWEEN 0 AND 32)
);

-- +goose Down
DROP TABLE session_agent_depth;
