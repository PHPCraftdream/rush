-- +goose Up
-- Rerun truncation (docs/reviews/2026-09-29-async-phase4-round1-rerun-design.md):
-- the id of the "started" tool-result message that acknowledged this job,
-- written by the ack gate's fused transaction (AnnounceStarted). Rerun voids
-- a job exactly when THIS message is among the rows it deleted -- unlike
-- tool_call_id, which a provider may reuse per response ("call_0") and which
-- the archive rename rewrites, the message id names exactly one history row.
-- NULL for rows announced before this migration (or via the plain
-- MarkAnnounced path): those fall back to the legacy tool_call_id arm.
-- +goose StatementBegin
ALTER TABLE async_jobs ADD COLUMN announce_message_id TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE async_jobs DROP COLUMN announce_message_id;
-- +goose StatementEnd
