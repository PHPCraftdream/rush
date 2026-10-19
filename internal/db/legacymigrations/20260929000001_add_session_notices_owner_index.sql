-- +goose Up
-- A9 (docs/reviews/2026-09-29-async-phase4-round1.md): session_notices had no
-- plain (owner) index. idx_session_notices_owner_pending/idx_session_notices_debt
-- are both partial (delivery='pending' / wake=1 AND reacted=0 AND
-- delivery!='void') and do not cover MarkSessionNoticesReactedForOwner (no
-- announced concept to narrow it further), ListReactedFailedSessionNoticesForOwner
-- (filters reacted_failed, not covered by either partial index), or
-- ListSessionNoticesForOwner (no predicate beyond owner at all) --
-- SQLite does not infer 'delivery != void' from 'delivery = done' or vice
-- versa across partial indexes, so all three fall back to a full table scan.
CREATE INDEX IF NOT EXISTS idx_session_notices_owner ON session_notices (owner);

-- +goose Down
DROP INDEX IF EXISTS idx_session_notices_owner;
