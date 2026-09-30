-- +goose Up
-- +goose StatementBegin
-- Phase 4 durable core (docs/plans/2026-09-28-async-phase4-durable-core.md,
-- rev 7). Replaces the never-released, rejected first implementation
-- (branch async-phase4, migration 20260929000001_add_async_job_ledger.sql):
-- no heartbeat/lease/delivered_at columns here -- liveness is an OS file
-- lock (DUR-5), not a clock comparison, and delivery is a durable outbox
-- state machine (DUR-2/DUR-3), not a single delivered_at timestamp.
--
-- async_hosts is DISPLAY ONLY (`sessions jobs`/`sessions hosts`): no FK from
-- async_jobs, no timestamp is ever consulted to decide whether a host is
-- alive (doc sec.3.6). Liveness is decided by trying to acquire the OS lock
-- on <data>/hosts/<id>.lock (host lock module, not this schema).
CREATE TABLE IF NOT EXISTS async_hosts (
    id         TEXT PRIMARY KEY, -- uuid v4, generated once per process lifetime at first claim
    pid        INTEGER NOT NULL, -- os.Getpid(), diagnostic/display only
    label      TEXT NOT NULL DEFAULT '', -- 'cli'|'web', for `rush sessions jobs`
    started_at INTEGER NOT NULL
);

-- async_jobs is the durable ledger for async tasks (bash/run_command/agent/
-- agentic_fetch). One row per (owner_session_id, tool_call_id). State is
-- changed ONLY by the transition CAS (DUR-1): `UPDATE ... WHERE
-- owner_session_id = ? AND tool_call_id = ? AND state = 'running'`.
--
-- host_id has NO REFERENCES clause on purpose (doc sec.3.6): async_hosts is
-- display-only bookkeeping, so a job row must be able to outlive (or simply
-- disagree in timing with) its host's own row without a FK ever blocking
-- either side's writes.
--
-- child_session_id has NO REFERENCES clause either (doc sec.3.8, ASYNC-01
-- carry-forward): the child session id is deterministic and written by
-- claim BEFORE the child session row exists, so a FK here would fail on
-- every fresh delegation.
CREATE TABLE IF NOT EXISTS async_jobs (
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    tool_call_id     TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('command', 'agent', 'fetch')),
    -- tool_name/timeout_seconds (step 3, docs/plans/2026-09-28-async-phase4-
    -- durable-core.md sec.5 step 3): the exact tool name ("bash"/
    -- "run_command"/"agent"/"agentic_fetch", finer-grained than `kind`'s
    -- three-way bucket) and the originally-requested timeout duration,
    -- filled once at Claim and never updated -- so the driver's pull
    -- (sec.3.3) can render the SAME notice text FormatAsyncCompletion
    -- already produces without adding a second formatter or re-deriving
    -- ToolName/TimeoutSeconds from anywhere else at pull time.
    tool_name        TEXT NOT NULL DEFAULT '',
    timeout_seconds  INTEGER NOT NULL DEFAULT 0,
    input_hash       TEXT NOT NULL, -- reusing an id with a DIFFERENT input is a distinct call, not an idempotent retry
    child_session_id TEXT,          -- set at claim for delegations (kind IN ('agent','fetch')); NULL for plain jobs
    origin_cli       INTEGER NOT NULL DEFAULT 0, -- recovery routing (CLI-owned vs web-owned)
    state            TEXT NOT NULL CHECK (
        state IN ('running', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')
    ),
    -- notice_kind carries the terminal-state cause into the eventual
    -- notice text/NoticeKind (message.NoticeKind's vocabulary, migration
    -- 20260928000001): '' for an ordinary finish, plus phase-2/3/4 values
    -- (timeout_wake_only, timeout_terminated, session_cancel, interrupted,
    -- wake_failed, ...). Written by the same transition transaction that
    -- sets state (doc sec.3.2/3.4).
    notice_kind       TEXT NOT NULL DEFAULT '',
    host_id           TEXT NOT NULL, -- lazily registered async_hosts.id of the claiming process; see host lock module
    announced         INTEGER NOT NULL DEFAULT 0, -- ack-gate (DUR-7): 1 only in the SAME tx as the "started" tool-result message insert
    delivery          TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,    -- session_notices/messages id the notice landed at, once delivery='done'
    wake              INTEGER NOT NULL DEFAULT 0, -- doc sec.3.4 "wake policy" table
    reacted           INTEGER NOT NULL DEFAULT 0, -- durable reaction debt marker (DUR-4); NOT derived from created_at/clock order
    -- wake_attempts/reacted_failed back doc sec.3.4's "settle by failure":
    -- a temporary provider failure after a wake-up call increments
    -- wake_attempts (counted in the row, not in memory, so it survives a
    -- process restart); at K=3, settle-by-failure sets reacted=1 AND
    -- reacted_failed=1 on the rows captured at the start of the failed turn
    -- (this table's columns only -- the separate "failed wake-up marker"
    -- notice the doc also describes, wake=0, is its own session_notices
    -- row, not a mutation of these rows' own wake field). reacted_failed
    -- distinguishes THIS closure from an ordinary step-persisted reaction --
    -- it is 1 ONLY when settle-by-failure wrote reacted=1, never for a real
    -- step's reaction -- so a child session whose debt closed this way can
    -- tell its parent "delegation failed" instead of "delegation succeeded
    -- with no output".
    wake_attempts     INTEGER NOT NULL DEFAULT 0,
    reacted_failed    INTEGER NOT NULL DEFAULT 0,
    deadline_at       INTEGER, -- explicit per-call timeout deadline, NULL if none
    timeout_kind      TEXT,    -- 'wake_only'|'terminate_and_wake', NULL if none
    result_summary    TEXT,    -- truncated tool output; NULL while running
    result_is_error   INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (owner_session_id, tool_call_id)
);

-- Doc sec.3.8's index list.
CREATE INDEX IF NOT EXISTS idx_async_jobs_host_id ON async_jobs (host_id);
CREATE INDEX IF NOT EXISTS idx_async_jobs_owner_pending ON async_jobs (owner_session_id) WHERE delivery = 'pending';
CREATE INDEX IF NOT EXISTS idx_async_jobs_running ON async_jobs (state) WHERE state = 'running';
-- ASYNC-01: at most one RUNNING delegation may claim a given child session.
CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_child_running
    ON async_jobs (child_session_id)
    WHERE state = 'running' AND child_session_id IS NOT NULL;
-- Doc sec.3.4's reaction-debt EXISTS check: "(owner) WHERE wake=1 AND
-- reacted=0 AND delivery<>'void'" on both tables, PLUS announced=1 for
-- async_jobs specifically (doc sec.3.4: "task rows with announced=1" --
-- review fix): an unannounced row can never produce a notice (DUR-7) and
-- must never count as debt, or the drain pull (which already requires
-- announced=1) skips it while the debt check still says "debt", producing
-- a wasted provider turn with an empty prompt and nothing to react to.
CREATE INDEX IF NOT EXISTS idx_async_jobs_debt
    ON async_jobs (owner_session_id)
    WHERE wake = 1 AND reacted = 0 AND delivery != 'void' AND announced = 1;
-- Retention scan (doc sec.3.7): terminal rows past a delivery outcome, old
-- enough to purge. Not in the doc's explicit index list but needed for the
-- same query shape as the removed idx_async_jobs_retention; harmless to add.
CREATE INDEX IF NOT EXISTS idx_async_jobs_retention
    ON async_jobs (state, updated_at)
    WHERE state != 'running';

-- session_notices carries notices that have no async_jobs row (supervision,
-- wake_failed marker, background SDK shell completion, wake_only timeout --
-- doc sec.2). Same outbox shape as async_jobs' delivery/wake/reacted fields.
CREATE TABLE IF NOT EXISTS session_notices (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    owner             TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind              TEXT NOT NULL, -- 'supervision', 'wake_failed', 'bg_shell_done', 'timeout_wake_only' (session.NoticeKind*)
    text              TEXT NOT NULL,
    -- No origin column: every session_notices call site (supervision,
    -- wake_failed, SDK background-shell completion, wake_only timeout)
    -- already built its notice via a plain context.Background()/
    -- WithTimeout ctx with no WithCallOrigin stamp before step 3, so the
    -- resulting message.Origin was always OriginUnspecified ("") -- adding
    -- a column to carry a value nothing ever produces would be unused
    -- schema. Job-row notices keep origin_cli on async_jobs (async_jobs.sql),
    -- unaffected by this.
    wake              INTEGER NOT NULL DEFAULT 0,
    delivery          TEXT NOT NULL DEFAULT 'pending' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    reacted           INTEGER NOT NULL DEFAULT 0,
    -- Same settle-by-failure pair as async_jobs (doc sec.3.4) -- see the
    -- comment there.
    wake_attempts     INTEGER NOT NULL DEFAULT 0,
    reacted_failed    INTEGER NOT NULL DEFAULT 0,
    -- job_tool_call_id pairs with `owner` (== async_jobs.owner_session_id)
    -- to name the async_jobs row this notice's "task still running?" void
    -- condition is checked against (doc sec.3.2); NULL for notices with no
    -- such condition (e.g. wake_failed).
    job_tool_call_id  TEXT,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_notices_owner_pending ON session_notices (owner) WHERE delivery = 'pending';
CREATE INDEX IF NOT EXISTS idx_session_notices_debt
    ON session_notices (owner)
    WHERE wake = 1 AND reacted = 0 AND delivery != 'void';
CREATE INDEX IF NOT EXISTS idx_session_notices_retention ON session_notices (updated_at) WHERE delivery IN ('done', 'void');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_session_notices_retention;
DROP INDEX IF EXISTS idx_session_notices_debt;
DROP INDEX IF EXISTS idx_session_notices_owner_pending;
DROP TABLE IF EXISTS session_notices;

DROP INDEX IF EXISTS idx_async_jobs_retention;
DROP INDEX IF EXISTS idx_async_jobs_debt;
DROP INDEX IF EXISTS idx_async_jobs_child_running;
DROP INDEX IF EXISTS idx_async_jobs_running;
DROP INDEX IF EXISTS idx_async_jobs_owner_pending;
DROP INDEX IF EXISTS idx_async_jobs_host_id;
DROP TABLE IF EXISTS async_jobs;

DROP TABLE IF EXISTS async_hosts;
-- +goose StatementEnd
