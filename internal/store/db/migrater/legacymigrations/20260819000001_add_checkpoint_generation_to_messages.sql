-- Fence mid-stream checkpoint writes by generation.
--
-- runTurn's checkpoint writer is torn down and replaced per step, and
-- stopCheckpoint waits only a bounded grace for the old writer before letting
-- the next one proceed. The old writer is cancelled, but that code exists
-- precisely for the case where the DB or filesystem does NOT honour the
-- context promptly -- so a stale write can still land after a newer one.
--
-- The existing guard (finished_at IS NULL) only protects a TERMINAL row. It
-- cannot tell two partial generations apart, so an older partial could
-- overwrite fresher parts. The terminal write at the end of the turn repairs
-- the row, but a crash in between leaves recovery with an outdated
-- checkpoint, which can replay tool actions that already ran.
--
-- Carrying the generation into the conditional UPDATE moves the check from
-- the writer's memory (where it races) into the statement itself.
--
-- Defaults to 0 so existing rows compare correctly against any writer's
-- generation, which starts at 1.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE messages ADD COLUMN checkpoint_generation INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE messages DROP COLUMN checkpoint_generation;
-- +goose StatementEnd
