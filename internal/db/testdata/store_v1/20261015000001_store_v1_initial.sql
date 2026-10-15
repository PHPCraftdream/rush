-- +goose Up
-- +goose StatementBegin
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    parent_session_id TEXT,
    title TEXT NOT NULL,
    message_count INTEGER NOT NULL DEFAULT 0 CHECK (message_count >= 0),
    prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK (prompt_tokens >= 0),
    completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK (completion_tokens >= 0),
    cost REAL NOT NULL DEFAULT 0.0 CHECK (cost >= 0.0),
    updated_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    summary_message_id TEXT,
    todos TEXT,
    smart_model_provider TEXT,
    smart_model_id TEXT,
    fast_model_provider TEXT,
    fast_model_id TEXT,
    system_prompt TEXT NOT NULL DEFAULT '',
    yolo_enabled INTEGER DEFAULT 0 NOT NULL,
    smart_model_reasoning_effort TEXT DEFAULT 'medium',
    fast_model_reasoning_effort TEXT DEFAULT 'medium',
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    ended_reason TEXT NOT NULL DEFAULT '',
    budget_max_cost REAL NOT NULL DEFAULT 0,
    budget_max_tokens INTEGER NOT NULL DEFAULT 0,
    budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
    deleted_todos TEXT NOT NULL DEFAULT '[]',
    parent_cost_accounted REAL NOT NULL DEFAULT 0,
    worker_model_provider TEXT,
    worker_model_id TEXT,
    worker_model_reasoning_effort TEXT,
    reviewer_model_provider TEXT,
    reviewer_model_id TEXT,
    reviewer_model_reasoning_effort TEXT,
    origin TEXT DEFAULT '' NOT NULL,
    cost_self REAL NOT NULL DEFAULT 0,
    cost_base REAL NOT NULL DEFAULT 0,
    cost_parent_id TEXT NOT NULL DEFAULT '',
    workspace_root TEXT NOT NULL DEFAULT '',
    git_branch TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_created_at ON sessions (created_at);
CREATE INDEX idx_sessions_cost_parent ON sessions (cost_parent_id);
CREATE INDEX idx_sessions_parent_session_id_updated_at ON sessions (parent_session_id, updated_at);
CREATE INDEX idx_sessions_parent_session_id_created_at ON sessions (parent_session_id, created_at);
CREATE INDEX idx_sessions_updated_at ON sessions (updated_at);
CREATE INDEX idx_sessions_workspace_updated ON sessions (workspace_root, updated_at) WHERE parent_session_id IS NULL;
CREATE TRIGGER update_sessions_updated_at AFTER UPDATE ON sessions BEGIN UPDATE sessions SET updated_at = strftime('%s', 'now') WHERE id = new.id; END;

CREATE TABLE files (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    path TEXT NOT NULL,
    content TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    UNIQUE (path, session_id, version)
);
CREATE INDEX idx_files_created_at ON files (created_at);
CREATE INDEX idx_files_path ON files (path);
CREATE INDEX idx_files_session_id_created_at ON files (session_id, created_at);
CREATE TRIGGER update_files_updated_at AFTER UPDATE ON files BEGIN UPDATE files SET updated_at = strftime('%s', 'now') WHERE id = new.id; END;

CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    role TEXT NOT NULL,
    parts TEXT NOT NULL DEFAULT '[]',
    model TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    finished_at INTEGER,
    provider TEXT,
    is_summary_message INTEGER DEFAULT 0 NOT NULL,
    pinned INTEGER DEFAULT 0 NOT NULL,
    hidden INTEGER DEFAULT 0 NOT NULL,
    reasoning_effort TEXT DEFAULT 'medium',
    auto_resumed INTEGER DEFAULT 0 NOT NULL,
    background_job_notice INTEGER DEFAULT 0 NOT NULL,
    input_tokens INTEGER,
    output_tokens INTEGER,
    reasoning_tokens INTEGER,
    cache_creation_tokens INTEGER,
    cache_read_tokens INTEGER,
    total_tokens INTEGER,
    cost_usd REAL,
    usage_provider TEXT,
    usage_model TEXT,
    cache_support TEXT,
    usage_estimated INTEGER,
    checkpoint_generation INTEGER NOT NULL DEFAULT 0,
    origin TEXT DEFAULT '' NOT NULL,
    notice_kind TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE
);
CREATE INDEX idx_messages_created_at ON messages (created_at);
CREATE INDEX idx_messages_session_id_created_at ON messages (session_id, created_at);
CREATE INDEX idx_messages_usage_session
ON messages (session_id, usage_model)
WHERE total_tokens IS NOT NULL;
CREATE INDEX idx_messages_unfinished_assistant
ON messages (session_id, created_at)
WHERE role = 'assistant' AND finished_at IS NULL;
CREATE TRIGGER update_messages_updated_at AFTER UPDATE ON messages BEGIN UPDATE messages SET updated_at = strftime('%s', 'now') WHERE id = new.id; END;
CREATE TRIGGER update_session_message_count_on_insert AFTER INSERT ON messages BEGIN UPDATE sessions SET message_count = message_count + 1 WHERE id = new.session_id; END;
CREATE TRIGGER update_session_message_count_on_delete AFTER DELETE ON messages BEGIN UPDATE sessions SET message_count = message_count - 1 WHERE id = old.session_id; END;

CREATE TABLE read_files (
    session_id TEXT NOT NULL CHECK (session_id != ''),
    path TEXT NOT NULL CHECK (path != ''),
    read_at INTEGER NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions (id) ON DELETE CASCADE,
    PRIMARY KEY (path, session_id)
);
CREATE INDEX idx_read_files_session_id ON read_files (session_id);
CREATE INDEX idx_read_files_path ON read_files (path);

CREATE TABLE session_permissions (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    action TEXT NOT NULL,
    path TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    enabled INTEGER DEFAULT 1 NOT NULL
);
CREATE INDEX idx_session_permissions_session_id ON session_permissions (session_id);
CREATE UNIQUE INDEX idx_session_permissions_uniq ON session_permissions (session_id, tool_name, action, path);
CREATE INDEX idx_session_permissions_match ON session_permissions (tool_name, action, path, enabled);

CREATE TABLE queue_tasks (
    id TEXT PRIMARY KEY,
    session_id TEXT,
    prompt TEXT NOT NULL,
    role TEXT,
    max_cost REAL,
    max_tokens INTEGER,
    timeout_sec INTEGER,
    status TEXT NOT NULL CHECK(status IN ('pending','running','done','failed','cancelled')),
    cost REAL DEFAULT 0,
    tokens INTEGER DEFAULT 0,
    exit_reason TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    workspace_root TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_queue_status ON queue_tasks (status, created_at);

CREATE TABLE pending_injects (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    message_id TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    interrupt INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_pending_injects_session_id ON pending_injects (session_id);

CREATE TABLE session_run_queue (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    call_data TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','leased','acked')),
    leased_by TEXT, leased_at INTEGER, lease_expires_at INTEGER,
    attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT,
    terminal_failure INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX idx_session_run_queue_status ON session_run_queue (status, created_at);
CREATE INDEX idx_session_run_queue_session_id ON session_run_queue (session_id);
CREATE INDEX idx_session_run_queue_lease_expiry ON session_run_queue (lease_expires_at) WHERE status = 'leased';

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

CREATE TABLE async_hosts (
    id TEXT PRIMARY KEY,
    pid INTEGER NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL
);
CREATE TABLE session_drivers (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    host_id TEXT NOT NULL,
    pid INTEGER NOT NULL,
    claimed_at INTEGER NOT NULL
);
CREATE INDEX idx_session_drivers_host_id ON session_drivers (host_id);

CREATE TABLE session_notices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    text TEXT NOT NULL,
    wake INTEGER NOT NULL DEFAULT 0,
    delivery TEXT NOT NULL DEFAULT 'pending' CHECK (delivery IN ('none', 'pending', 'done', 'void')),
    notice_message_id TEXT,
    reacted INTEGER NOT NULL DEFAULT 0,
    wake_attempts INTEGER NOT NULL DEFAULT 0,
    reacted_failed INTEGER NOT NULL DEFAULT 0,
    job_tool_call_id TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX idx_session_notices_owner_pending ON session_notices (owner) WHERE delivery = 'pending';
CREATE INDEX idx_session_notices_debt ON session_notices (owner) WHERE wake = 1 AND reacted = 0 AND delivery != 'void';
CREATE INDEX idx_session_notices_retention ON session_notices (updated_at) WHERE delivery IN ('done', 'void');
CREATE INDEX idx_session_notices_owner ON session_notices (owner);

CREATE TABLE wake_schedules (
    id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('once', 'loop')),
    message TEXT NOT NULL,
    next_run_at INTEGER NOT NULL,
    every_ms INTEGER NOT NULL DEFAULT 0,
    max_runs INTEGER,
    until_at INTEGER,
    state TEXT NOT NULL CHECK (state IN ('active', 'done', 'cancelled')),
    occurrence INTEGER NOT NULL DEFAULT 0,
    lease_owner TEXT,
    lease_expires_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX idx_wake_schedules_due ON wake_schedules (state, next_run_at);

-- Batch tables preserve the composite identity and state constraints from the projection migration.
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
    runner_id TEXT NOT NULL DEFAULT '',
    response_envelope TEXT,
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
CREATE TABLE batch_control_requests (
    request_id TEXT PRIMARY KEY,
    owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    root_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    host_id TEXT NOT NULL,
    operation TEXT NOT NULL DEFAULT 'stop' CHECK (operation = 'stop'),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'answered')),
    answer_code TEXT NOT NULL DEFAULT '',
    answer_sequence INTEGER,
    created_at INTEGER NOT NULL,
    answered_at INTEGER,
    runner_id TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (root_id, node_id, owner_session_id, host_id) REFERENCES batch_nodes (root_id, node_id, owner_session_id, host_id) ON DELETE CASCADE,
    CHECK ((status = 'pending' AND answered_at IS NULL AND answer_sequence IS NULL AND answer_code = '') OR (status = 'answered' AND answered_at IS NOT NULL AND answer_sequence IS NOT NULL AND answer_sequence > 0))
);
CREATE INDEX idx_batch_control_requests_pending ON batch_control_requests (host_id, root_id, created_at, request_id) WHERE status = 'pending';
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
DROP TABLE batch_control_requests;
DROP TABLE batch_nodes;
DROP TABLE wake_schedules;
DROP TABLE session_notices;
DROP TABLE session_drivers;
DROP TABLE async_hosts;
DROP TABLE orphan_call_outbox;
DROP TABLE session_run_queue;
DROP TABLE pending_injects;
DROP TABLE queue_tasks;
DROP TABLE session_permissions;
DROP TABLE read_files;
DROP TABLE messages;
DROP TABLE files;
DROP TABLE sessions;
-- +goose StatementEnd
