package dbtest

import (
	"context"
	"database/sql"
	"strings"
)

// FinalizationFencingSchema is the production DDL of the tables the
// finalization-ancestry fence reads and writes inside every productive-ingress
// transaction (batch_nodes, orphan_call_outbox, session_agent_finalization*).
// Hand-written partial fixture schemas run it after their own setup via
// ApplyFinalizationFencingSchema. It is copied verbatim from the migrations;
// the drift guard in internal/db fails when it diverges. Foreign keys reference
// only sessions and batch_nodes, so no other table is required.
const FinalizationFencingSchema = `
CREATE TABLE batch_nodes (
    node_id TEXT PRIMARY KEY,
    root_id TEXT NOT NULL,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    host_id TEXT NOT NULL,
    parent_id TEXT,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('task', 'batch')),
    tool_call_id TEXT,
    input_hash TEXT,
    announced INTEGER NOT NULL DEFAULT 0 CHECK (announced IN (0, 1)),
    announce_message_id TEXT,
    origin_cli INTEGER NOT NULL DEFAULT 0 CHECK (origin_cli IN (0, 1)),
    mode TEXT CHECK (mode IN ('parallel', 'sequential')),
    on_fail TEXT CHECK (on_fail IN ('', 'stop', 'continue')),
    notify TEXT CHECK (notify IN ('', 'all', 'each')),
    max_parallel INTEGER NOT NULL DEFAULT 0 CHECK (max_parallel >= 0),
    task_kind TEXT,
    task_group TEXT NOT NULL DEFAULT '',
    task_payload TEXT,
    timeout_ns INTEGER NOT NULL DEFAULT 0 CHECK (timeout_ns >= 0),
    child_session_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('pending', 'starting', 'running', 'cancelling', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped')),
    cause TEXT NOT NULL DEFAULT '' CHECK (cause IN ('', 'stop', 'timeout', 'host_closing', 'stop_on_fail')),
    token INTEGER NOT NULL DEFAULT 0 CHECK (token >= 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    control_state TEXT NOT NULL DEFAULT 'none' CHECK (control_state IN ('none', 'pending', 'acked', 'failed')),
    control_error TEXT NOT NULL DEFAULT '',
    control_pending INTEGER NOT NULL DEFAULT 0 CHECK (control_pending IN (0, 1)),
    outcome_state TEXT NOT NULL DEFAULT '' CHECK (outcome_state IN ('', 'completed', 'failed', 'cancelled', 'timed_out', 'interrupted')),
    outcome_code TEXT NOT NULL DEFAULT '',
    outcome_summary TEXT NOT NULL DEFAULT '',
    response_envelope_ref TEXT,
    root_sequence INTEGER NOT NULL DEFAULT 0 CHECK (root_sequence >= 0),
    projection_sequence INTEGER NOT NULL DEFAULT 0 CHECK (projection_sequence >= 0),
    notice_selected INTEGER NOT NULL DEFAULT 0 CHECK (notice_selected IN (0, 1)),
    notice_kind TEXT CHECK (notice_kind IN ('task', 'batch', 'root')),
    notice_state TEXT CHECK (notice_state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped')),
    notice_cause TEXT CHECK (notice_cause IN ('', 'stop', 'timeout', 'host_closing', 'stop_on_fail')),
    notice_sequence INTEGER,
    notice_ordinal INTEGER,
    notice_pending INTEGER NOT NULL DEFAULT 0 CHECK (notice_pending >= 0),
    notice_active INTEGER NOT NULL DEFAULT 0 CHECK (notice_active >= 0),
    notice_completed INTEGER NOT NULL DEFAULT 0 CHECK (notice_completed >= 0),
    notice_failed INTEGER NOT NULL DEFAULT 0 CHECK (notice_failed >= 0),
    notice_cancelled INTEGER NOT NULL DEFAULT 0 CHECK (notice_cancelled >= 0),
    notice_timed_out INTEGER NOT NULL DEFAULT 0 CHECK (notice_timed_out >= 0),
    notice_interrupted INTEGER NOT NULL DEFAULT 0 CHECK (notice_interrupted >= 0),
    notice_skipped INTEGER NOT NULL DEFAULT 0 CHECK (notice_skipped >= 0),
    delivery TEXT NOT NULL DEFAULT 'none' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    wake INTEGER NOT NULL DEFAULT 0 CHECK (wake IN (0, 1)),
    reacted INTEGER NOT NULL DEFAULT 0 CHECK (reacted IN (0, 1)),
    wake_attempts INTEGER NOT NULL DEFAULT 0 CHECK (wake_attempts >= 0),
    reacted_failed INTEGER NOT NULL DEFAULT 0 CHECK (reacted_failed IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    runner_id TEXT NOT NULL DEFAULT '',
    response_envelope TEXT,
    UNIQUE (root_id, node_id, owner_session_id, host_id),
    UNIQUE (root_id, parent_id, ordinal),
    FOREIGN KEY (root_id, root_id, owner_session_id, host_id)
        REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE,
    FOREIGN KEY (root_id, parent_id, owner_session_id, host_id)
        REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE,
    CHECK ((node_id = root_id AND parent_id IS NULL AND kind = 'batch' AND ordinal = 0
            AND tool_call_id IS NOT NULL AND input_hash IS NOT NULL)
        OR (node_id != root_id AND parent_id IS NOT NULL AND tool_call_id IS NULL
            AND input_hash IS NULL AND announced = 0 AND announce_message_id IS NULL
            AND root_sequence = 0)),
    CHECK ((kind = 'batch' AND mode IS NOT NULL AND task_kind IS NULL)
        OR (kind = 'task' AND task_kind IS NOT NULL AND mode IS NULL AND on_fail IS NULL AND notify IS NULL)),
    CHECK (announced = 0 OR announce_message_id IS NOT NULL),
    CHECK ((notice_selected = 0 AND notice_kind IS NULL AND notice_state IS NULL
            AND notice_cause IS NULL AND notice_sequence IS NULL AND notice_ordinal IS NULL
            AND delivery = 'none' AND wake = 0)
        OR (notice_selected = 1 AND notice_kind IS NOT NULL AND notice_state IS NOT NULL
            AND notice_cause IS NOT NULL AND notice_sequence IS NOT NULL AND notice_sequence > 0
            AND notice_ordinal IS NOT NULL AND notice_ordinal >= 0 AND delivery != 'none'))
);
CREATE UNIQUE INDEX idx_batch_nodes_live_key ON batch_nodes (owner_session_id, tool_call_id)
    WHERE node_id = root_id AND state IN ('pending', 'starting', 'running', 'cancelling');
CREATE INDEX idx_batch_nodes_host_live ON batch_nodes (host_id, root_id)
    WHERE node_id = root_id AND state IN ('pending', 'starting', 'running', 'cancelling');
CREATE INDEX idx_batch_nodes_owner_roots ON batch_nodes (owner_session_id, created_at, root_id)
    WHERE node_id = root_id;
CREATE INDEX idx_batch_nodes_parent ON batch_nodes (root_id, parent_id, ordinal);
CREATE INDEX idx_batch_nodes_pending_notice ON batch_nodes (owner_session_id, root_id, notice_sequence, notice_ordinal)
    WHERE notice_selected = 1 AND delivery = 'pending';
CREATE INDEX idx_batch_nodes_debt ON batch_nodes (owner_session_id, root_id)
    WHERE notice_selected = 1 AND wake = 1 AND reacted = 0 AND delivery != 'void';
CREATE TABLE orphan_call_outbox (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    call_data TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','processing','done','failed')),
    attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 5,
    last_error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_orphan_call_outbox_status ON orphan_call_outbox (status, created_at);
CREATE INDEX idx_orphan_call_outbox_session_id ON orphan_call_outbox (session_id);

CREATE TABLE session_agent_finalizations (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    logical_call_id TEXT NOT NULL CHECK (logical_call_id <> ''),
    generation INTEGER NOT NULL CHECK (generation > 0),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    phase TEXT NOT NULL DEFAULT 'executing' CHECK (phase IN ('executing', 'waiting_work', 'composing', 'accepted', 'failed', 'cancelled', 'interrupted')),
    node_id TEXT NOT NULL DEFAULT '',
    node_token INTEGER NOT NULL DEFAULT 0 CHECK (node_token >= 0),
    candidate_message_id TEXT NOT NULL DEFAULT '',
    candidate_hash TEXT NOT NULL DEFAULT '',
    CHECK ((node_id = '' AND node_token = 0) OR (node_id <> '' AND node_token > 0)),
    PRIMARY KEY (session_id, logical_call_id, generation)
);

CREATE UNIQUE INDEX session_agent_finalizations_active_owner
ON session_agent_finalizations(session_id)
WHERE phase IN ('executing', 'waiting_work', 'composing');

CREATE TABLE session_agent_finalization_tickets (
    session_id TEXT NOT NULL,
    logical_call_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    ticket_id TEXT NOT NULL CHECK (ticket_id <> ''),
    released INTEGER NOT NULL DEFAULT 0 CHECK (released IN (0, 1)),
    PRIMARY KEY (session_id, logical_call_id, generation, ticket_id),
    FOREIGN KEY (session_id, logical_call_id, generation)
        REFERENCES session_agent_finalizations(session_id, logical_call_id, generation) ON DELETE CASCADE
);

CREATE INDEX session_agent_finalization_tickets_open
ON session_agent_finalization_tickets(session_id, logical_call_id, generation)
WHERE released = 0;

CREATE TABLE session_agent_finalization_continuations (
    row_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    logical_call_id TEXT NOT NULL CHECK (logical_call_id <> ''),
    payload_hash TEXT NOT NULL CHECK (payload_hash <> ''),
    identities TEXT NOT NULL,
    ancestry TEXT NOT NULL,
    lease_token TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    consumed INTEGER NOT NULL DEFAULT 0 CHECK (consumed IN (0, 1))
);

`

var finalizationIfNotExists = strings.NewReplacer(
	"CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ",
	"CREATE UNIQUE INDEX ", "CREATE UNIQUE INDEX IF NOT EXISTS ",
	"CREATE INDEX ", "CREATE INDEX IF NOT EXISTS ",
)

// execer is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ApplyFinalizationFencingSchema creates the FinalizationFencingSchema tables
// and indexes on e, skipping any the fixture already created.
func ApplyFinalizationFencingSchema(ctx context.Context, e execer) error {
	_, err := e.ExecContext(ctx, finalizationIfNotExists.Replace(FinalizationFencingSchema))
	return err
}
