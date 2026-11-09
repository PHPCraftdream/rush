-- +goose Up
CREATE TABLE session_agent_model_role (
    session_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    requested_role TEXT NOT NULL CHECK (requested_role IN ('', 'smart', 'fast', 'worker', 'reviewer'))
);

-- +goose Down
DROP TABLE session_agent_model_role;
