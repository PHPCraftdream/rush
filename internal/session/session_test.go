package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/charmbracelet/crush/internal/db"
)

func newTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	// Use file-based database in temp dir to avoid connection pool issues with :memory:
	// Each connection to :memory: creates a separate database; when sql.Open recycles
	// a connection (e.g., after ErrBadConn from context cancellation), data is lost.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { sqlDB.Close() })

	// Run migrations
	_, err = sqlDB.ExecContext(context.Background(), `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			summary_message_id TEXT,
			todos TEXT,
			smart_model_provider TEXT,
			smart_model_id TEXT,
			smart_model_reasoning_effort TEXT DEFAULT 'medium',
			fast_model_provider TEXT,
			fast_model_id TEXT,
			fast_model_reasoning_effort TEXT DEFAULT 'medium',
			worker_model_provider TEXT,
			worker_model_id TEXT,
			worker_model_reasoning_effort TEXT,
			reviewer_model_provider TEXT,
			reviewer_model_id TEXT,
			reviewer_model_reasoning_effort TEXT,
			system_prompt TEXT DEFAULT '',
			yolo_enabled INTEGER NOT NULL DEFAULT 0,
			deleted_todos TEXT NOT NULL DEFAULT '[]',
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0
		);

		CREATE TABLE session_permissions (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			action TEXT NOT NULL,
			path TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL
		);

		CREATE TABLE messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			provider TEXT,
			reasoning_effort TEXT DEFAULT 'medium',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER,
			is_summary_message INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			hidden INTEGER NOT NULL DEFAULT 0,
			auto_resumed INTEGER NOT NULL DEFAULT 0,
			background_job_notice INTEGER NOT NULL DEFAULT 0,
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
			checkpoint_generation INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE files (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			content TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(path, session_id, version)
		);

		CREATE TABLE read_files (
			session_id TEXT NOT NULL,
			path TEXT NOT NULL,
			read_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, path)
		);

		CREATE TABLE pending_injects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			content TEXT NOT NULL,
			interrupt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);

		CREATE TABLE session_run_queue (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			call_data TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			leased_by TEXT,
			leased_at INTEGER,
			lease_expires_at INTEGER,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			terminal_failure INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE TABLE orphan_call_outbox (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
			call_data TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('pending', 'processing', 'done', 'failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			last_error TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);

		CREATE INDEX idx_files_session_id ON files(session_id);
		CREATE INDEX idx_files_path ON files(path);
		CREATE INDEX idx_messages_session_id ON messages(session_id);
		CREATE INDEX idx_pending_injects_session_id ON pending_injects(session_id);
		CREATE INDEX idx_session_run_queue_session_status ON session_run_queue(session_id, status);
		CREATE INDEX idx_session_run_queue_created_at ON session_run_queue(created_at);
		CREATE INDEX idx_orphan_call_outbox_status ON orphan_call_outbox(status, created_at);
		CREATE INDEX idx_orphan_call_outbox_session_id ON orphan_call_outbox(session_id);
	`)
	require.NoError(t, err)

	return sqlDB, db.New(sqlDB)
}
