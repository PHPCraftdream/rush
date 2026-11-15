-- +goose Up
ALTER TABLE messages ADD COLUMN notice_kind TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN notice_kind;
