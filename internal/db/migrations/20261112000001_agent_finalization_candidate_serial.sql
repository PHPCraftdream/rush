-- +goose Up
ALTER TABLE session_agent_finalizations ADD COLUMN candidate_serial INTEGER NOT NULL DEFAULT 0 CHECK (candidate_serial >= 0);

-- +goose Down
ALTER TABLE session_agent_finalizations DROP COLUMN candidate_serial;
