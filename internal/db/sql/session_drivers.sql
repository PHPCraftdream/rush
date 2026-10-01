-- name: GetSessionDriver :one
-- The durable external-driver marker for a session (migration
-- 20260929000004): which host's `rush run` loop drives it.
SELECT * FROM session_drivers WHERE session_id = ?;

-- name: InsertSessionDriver :execrows
-- First claim of a session's driver marker. DO NOTHING on conflict: the
-- caller reads rows-affected and, on 0, re-observes who holds the row.
INSERT INTO session_drivers (session_id, host_id, pid, claimed_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (session_id) DO NOTHING;

-- name: TakeOverSessionDriver :execrows
-- CAS takeover of a marker whose host the caller proved dead: succeeds only
-- while the row still names exactly that dead host, so of N concurrent
-- takers exactly one wins (rows-affected 1).
UPDATE session_drivers SET host_id = @host_id, pid = @pid, claimed_at = @claimed_at
WHERE session_id = @session_id AND host_id = @expected_host_id;

-- name: DeleteSessionDriver :execrows
-- A driver releases only its OWN claim (scoped to its host id).
DELETE FROM session_drivers WHERE session_id = ? AND host_id = ?;

-- name: DeleteSessionDriversForHost :execrows
-- Host exit (AsyncJobStore.Close) or dead-host purge: every marker naming
-- the host goes with it.
DELETE FROM session_drivers WHERE host_id = ?;

-- name: ListSessionDriverHostIDs :many
-- Candidate set for the dead-driver purge; liveness itself is decided by the
-- host lock module, not by this query.
SELECT DISTINCT host_id FROM session_drivers;

-- name: ListSessionDrivers :many
-- Every marker row, liveness undecided: the activity reader decides it once
-- per distinct host (host lock module), keeping dead-host markers visible
-- for the crashed verdict where LiveSessionDrivers drops them.
SELECT * FROM session_drivers;
