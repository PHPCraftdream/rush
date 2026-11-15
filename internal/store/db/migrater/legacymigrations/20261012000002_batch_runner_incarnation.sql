-- +goose Up
-- Additive R5 repairs: Runner incarnation fencing, response envelope storage
-- and a rootless Runner lifecycle checkpoint. Re-running a CREATE/DROP that
-- already applied is rejected by goose's versioning; each statement adds
-- exactly what its Down removes.
-- +goose StatementBegin
ALTER TABLE batch_nodes ADD COLUMN runner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE batch_nodes ADD COLUMN response_envelope TEXT;
ALTER TABLE batch_control_requests ADD COLUMN runner_id TEXT NOT NULL DEFAULT '';
CREATE TABLE batch_runner_checkpoints (
    host_id TEXT NOT NULL,
    runner_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence >= 0),
    event_type TEXT NOT NULL DEFAULT '',
    final INTEGER NOT NULL DEFAULT 0 CHECK (final IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (host_id, runner_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE batch_runner_checkpoints;
ALTER TABLE batch_control_requests DROP COLUMN runner_id;
ALTER TABLE batch_nodes DROP COLUMN response_envelope;
ALTER TABLE batch_nodes DROP COLUMN runner_id;
-- +goose StatementEnd
