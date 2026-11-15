-- +goose Up
ALTER TABLE sessions ADD COLUMN origin TEXT DEFAULT '' NOT NULL;
ALTER TABLE messages ADD COLUMN origin TEXT DEFAULT '' NOT NULL;

-- +goose Down
ALTER TABLE sessions DROP COLUMN origin;
ALTER TABLE messages DROP COLUMN origin;
