-- Rename the two original model slots from large/small to smart/fast, so the
-- schema uses the same vocabulary as everything above it. `--role smart` /
-- `--role fast` were already the CLI's words for these slots and were
-- translated to large/small at the boundary; two names for one thing is what
-- this removes.
--
-- RENAME COLUMN, not add-copy-drop: the existing values are live session
-- state (which model each session is pinned to, and at what reasoning
-- effort), and a rebuild would risk them for no benefit. SQLite has
-- supported ALTER TABLE ... RENAME COLUMN since 3.25.
--
-- The worker/reviewer slots are untouched — they were named after their role
-- from the start, which is exactly the convention these four columns now
-- follow.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions RENAME COLUMN large_model_provider TO smart_model_provider;
ALTER TABLE sessions RENAME COLUMN large_model_id TO smart_model_id;
ALTER TABLE sessions RENAME COLUMN large_model_reasoning_effort TO smart_model_reasoning_effort;
ALTER TABLE sessions RENAME COLUMN small_model_provider TO fast_model_provider;
ALTER TABLE sessions RENAME COLUMN small_model_id TO fast_model_id;
ALTER TABLE sessions RENAME COLUMN small_model_reasoning_effort TO fast_model_reasoning_effort;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE sessions RENAME COLUMN smart_model_provider TO large_model_provider;
ALTER TABLE sessions RENAME COLUMN smart_model_id TO large_model_id;
ALTER TABLE sessions RENAME COLUMN smart_model_reasoning_effort TO large_model_reasoning_effort;
ALTER TABLE sessions RENAME COLUMN fast_model_provider TO small_model_provider;
ALTER TABLE sessions RENAME COLUMN fast_model_id TO small_model_id;
ALTER TABLE sessions RENAME COLUMN fast_model_reasoning_effort TO small_model_reasoning_effort;
-- +goose StatementEnd
