package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var storeV1Tables = []string{
	"sessions", "messages", "files", "read_files", "session_permissions", "queue_tasks",
	"pending_injects", "session_run_queue", "orphan_call_outbox", "async_hosts",
	"session_drivers", "session_notices", "wake_schedules", "batch_nodes",
	"batch_control_requests", "batch_runner_checkpoints",
}

func openFixture(t *testing.T, fixture bool) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	conn, err := openDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	conn.SetMaxOpenConns(1)
	var migrations fs.FS
	if fixture {
		migrations = storeV1FixtureMigrations()
	} else {
		migrations, err = migrationFS()
		require.NoError(t, err)
	}
	require.NoError(t, conn.PingContext(context.Background()))
	require.NoError(t, migrateLocked(context.Background(), conn, path, migrations, MigrateOptions{MayMigrate: true}))
	return conn
}

func sqliteObjects(t *testing.T, conn *sql.DB, exclude map[string]bool) map[string]string {
	t.Helper()
	rows, err := conn.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer rows.Close()
	objects := make(map[string]string)
	for rows.Next() {
		var typ, name, table string
		var ddl sql.NullString
		require.NoError(t, rows.Scan(&typ, &name, &table, &ddl))
		if name == "goose_db_version" || name == "sqlite_sequence" || exclude[table] {
			continue
		}
		objects[typ+":"+name] = normalizeSchemaSQL(ddl.String)
	}
	require.NoError(t, rows.Err())
	return objects
}

func normalizeSchemaSQL(ddl string) string {
	var tokens []string
	for i := 0; i < len(ddl); {
		c := ddl[i]
		if c == '-' && i+1 < len(ddl) && ddl[i+1] == '-' {
			for i < len(ddl) && ddl[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(ddl) && ddl[i+1] == '*' {
			i += 2
			for i+1 < len(ddl) && !(ddl[i] == '*' && ddl[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote := c
			start := i
			i++
			for i < len(ddl) {
				if ddl[i] == quote {
					i++
					if i < len(ddl) && ddl[i] == quote {
						i++
						continue
					}
					break
				}
				i++
			}
			if quote == '\'' {
				tokens = append(tokens, ddl[start:i])
			} else {
				tokens = append(tokens, strings.ToLower(ddl[start+1:i-1]))
			}
			continue
		}
		if c == '[' {
			start := i + 1
			i++
			for i < len(ddl) && ddl[i] != ']' {
				i++
			}
			tokens = append(tokens, strings.ToLower(ddl[start:i]))
			if i < len(ddl) {
				i++
			}
			continue
		}
		if c <= ' ' {
			i++
			continue
		}
		if isSchemaPunctuation(c) || c == '+' || c == '!' {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		start := i
		for i < len(ddl) && ddl[i] > ' ' && !isSchemaPunctuation(ddl[i]) && ddl[i] != '!' && ddl[i] != '+' && ddl[i] != '\'' && ddl[i] != '"' && ddl[i] != '`' && ddl[i] != '[' && ddl[i] != ']' {
			i++
		}
		tokens = append(tokens, strings.ToLower(ddl[start:i]))
	}
	var canonical []string
	for i := 0; i < len(tokens); i++ {
		if i+2 < len(tokens) && tokens[i] == "if" && tokens[i+1] == "not" && tokens[i+2] == "exists" {
			i += 2
			continue
		}
		canonical = append(canonical, tokens[i])
	}
	return strings.Join(canonical, " ")
}

func isSchemaPunctuation(c byte) bool {
	switch c {
	case '(', ')', ',', ';', '=', '<', '>':
		return true
	}
	return false
}

func TestStoreV1FixtureStructure(t *testing.T) {
	conn := openFixture(t, true)
	for _, table := range storeV1Tables {
		var name string
		require.NoError(t, conn.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name), table)
	}
	expectedColumns := map[string][]string{
		"sessions":                 {"id", "parent_session_id", "title", "message_count", "prompt_tokens", "completion_tokens", "cost", "updated_at", "created_at", "summary_message_id", "todos", "smart_model_provider", "smart_model_id", "fast_model_provider", "fast_model_id", "system_prompt", "yolo_enabled", "smart_model_reasoning_effort", "fast_model_reasoning_effort", "cancel_requested", "ended_reason", "budget_max_cost", "budget_max_tokens", "budget_timeout_sec", "deleted_todos", "parent_cost_accounted", "worker_model_provider", "worker_model_id", "worker_model_reasoning_effort", "reviewer_model_provider", "reviewer_model_id", "reviewer_model_reasoning_effort", "origin", "cost_self", "cost_base", "cost_parent_id", "workspace_root", "git_branch"},
		"messages":                 {"id", "session_id", "role", "parts", "model", "created_at", "updated_at", "finished_at", "provider", "is_summary_message", "pinned", "hidden", "reasoning_effort", "auto_resumed", "background_job_notice", "input_tokens", "output_tokens", "reasoning_tokens", "cache_creation_tokens", "cache_read_tokens", "total_tokens", "cost_usd", "usage_provider", "usage_model", "cache_support", "usage_estimated", "checkpoint_generation", "origin", "notice_kind"},
		"files":                    {"id", "session_id", "path", "content", "version", "created_at", "updated_at"},
		"read_files":               {"session_id", "path", "read_at"},
		"session_permissions":      {"id", "session_id", "tool_name", "action", "path", "created_at", "enabled"},
		"queue_tasks":              {"id", "session_id", "prompt", "role", "max_cost", "max_tokens", "timeout_sec", "status", "cost", "tokens", "exit_reason", "created_at", "started_at", "finished_at", "workspace_root"},
		"pending_injects":          {"id", "session_id", "message_id", "content", "interrupt", "created_at"},
		"session_run_queue":        {"id", "session_id", "call_data", "status", "leased_by", "leased_at", "lease_expires_at", "attempts", "last_error", "terminal_failure", "created_at", "updated_at"},
		"orphan_call_outbox":       {"id", "session_id", "call_data", "status", "attempts", "max_attempts", "last_error", "created_at", "updated_at"},
		"async_hosts":              {"id", "pid", "label", "started_at"},
		"session_drivers":          {"session_id", "host_id", "pid", "claimed_at"},
		"session_notices":          {"id", "owner", "kind", "text", "wake", "delivery", "notice_message_id", "reacted", "wake_attempts", "reacted_failed", "job_tool_call_id", "created_at", "updated_at"},
		"wake_schedules":           {"id", "owner_session_id", "kind", "message", "next_run_at", "every_ms", "max_runs", "until_at", "state", "occurrence", "lease_owner", "lease_expires_at", "created_at", "updated_at"},
		"batch_nodes":              {"node_id", "root_id", "owner_session_id", "host_id", "parent_id", "ordinal", "kind", "tool_call_id", "input_hash", "announced", "announce_message_id", "origin_cli", "mode", "on_fail", "notify", "max_parallel", "task_kind", "task_group", "task_payload", "timeout_ns", "child_session_id", "state", "cause", "token", "attempts", "control_state", "control_error", "control_pending", "outcome_state", "outcome_code", "outcome_summary", "response_envelope_ref", "response_envelope", "runner_id", "root_sequence", "projection_sequence", "notice_selected", "notice_kind", "notice_state", "notice_cause", "notice_sequence", "notice_ordinal", "notice_pending", "notice_active", "notice_completed", "notice_failed", "notice_cancelled", "notice_timed_out", "notice_interrupted", "notice_skipped", "delivery", "notice_message_id", "wake", "reacted", "wake_attempts", "reacted_failed", "created_at", "updated_at"},
		"batch_control_requests":   {"request_id", "owner_session_id", "root_id", "node_id", "host_id", "runner_id", "operation", "status", "answer_code", "answer_sequence", "created_at", "answered_at"},
		"batch_runner_checkpoints": {"host_id", "runner_id", "sequence", "event_type", "final", "created_at", "updated_at"},
	}
	for table, want := range expectedColumns {
		rows, err := conn.Query(`PRAGMA table_info(` + table + `)`)
		require.NoError(t, err)
		var got []string
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var def any
			require.NoError(t, rows.Scan(&cid, &name, &typ, &notnull, &def, &pk))
			got = append(got, name)
		}
		require.NoError(t, rows.Close())
		require.ElementsMatch(t, want, got, table)
	}
	objects := sqliteObjects(t, conn, nil)
	tables := make(map[string]bool)
	for key := range objects {
		if strings.HasPrefix(key, "table:") {
			tables[strings.TrimPrefix(key, "table:")] = true
		}
	}
	require.Equal(t, len(storeV1Tables), len(tables), "only retained tables may exist")
	for _, table := range storeV1Tables {
		require.True(t, tables[table], table)
	}
	for _, absent := range []string{"async_jobs", "async_jobs_new", "async_jobs_old_schema"} {
		var count int
		require.NoError(t, conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, absent).Scan(&count))
		require.Zero(t, count, absent)
	}
	var isNew bool
	rows, err := conn.Query(`PRAGMA table_info(files)`)
	require.NoError(t, err)
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notnull, &def, &pk))
		isNew = isNew || name == "is_new"
	}
	require.NoError(t, rows.Close())
	require.False(t, isNew)
	for _, object := range []string{
		"idx_sessions_created_at", "idx_sessions_cost_parent", "idx_sessions_parent_session_id_updated_at", "idx_sessions_parent_session_id_created_at", "idx_sessions_updated_at", "idx_sessions_workspace_updated",
		"idx_files_created_at", "idx_files_path", "idx_files_session_id_created_at",
		"idx_messages_created_at", "idx_messages_session_id_created_at", "idx_messages_usage_session", "idx_messages_unfinished_assistant",
		"idx_read_files_session_id", "idx_read_files_path", "idx_session_permissions_session_id", "idx_session_permissions_uniq", "idx_session_permissions_match",
		"idx_queue_status", "idx_pending_injects_session_id", "idx_session_run_queue_status", "idx_session_run_queue_session_id", "idx_session_run_queue_lease_expiry",
		"idx_orphan_call_outbox_status", "idx_orphan_call_outbox_session_id", "idx_session_drivers_host_id",
		"idx_session_notices_owner_pending", "idx_session_notices_debt", "idx_session_notices_retention", "idx_session_notices_owner", "idx_wake_schedules_due",
		"idx_batch_nodes_live_key", "idx_batch_nodes_host_live", "idx_batch_nodes_owner_roots", "idx_batch_nodes_parent", "idx_batch_nodes_pending_notice", "idx_batch_nodes_debt", "idx_batch_control_requests_pending",
		"update_sessions_updated_at", "update_files_updated_at", "update_messages_updated_at", "update_session_message_count_on_insert", "update_session_message_count_on_delete",
	} {
		var count int
		require.NoError(t, conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, object).Scan(&count))
		require.Equal(t, 1, count, object)
	}
}

func TestStoreV1FixtureMatchesEmbeddedSchemaMinusOmittedObjects(t *testing.T) {
	fixture := openFixture(t, true)
	embedded := openFixture(t, false)
	omitted := map[string]bool{}
	rows, err := embedded.Query(`SELECT name FROM sqlite_master WHERE tbl_name='async_jobs'`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		omitted[name] = true
	}
	require.NoError(t, rows.Close())
	fixtureObjects := sqliteObjects(t, fixture, nil)
	embeddedObjects := sqliteObjects(t, embedded, omitted)
	// Columns are sorted only after tokenizing table elements outside quoted text; constraints stay as independent exact elements.
	for key, ddl := range fixtureObjects {
		if strings.HasPrefix(key, "table:") {
			fixtureObjects[key] = normalizeTableColumnOrder(ddl)
		}
	}
	for key, ddl := range embeddedObjects {
		if strings.HasPrefix(key, "table:") {
			embeddedObjects[key] = normalizeTableColumnOrder(ddl)
		}
	}
	for key := range fixtureObjects {
		if !strings.HasPrefix(key, "table:") && !strings.HasPrefix(key, "index:") && !strings.HasPrefix(key, "trigger:") {
			t.Errorf("unexpected sqlite_master object: %s", key)
		}
	}
	for key := range embeddedObjects {
		if !strings.HasPrefix(key, "table:") && !strings.HasPrefix(key, "index:") && !strings.HasPrefix(key, "trigger:") {
			t.Errorf("unexpected embedded sqlite_master object: %s", key)
		}
	}
	for _, tableKey := range []string{"table:sessions", "table:messages", "table:files", "table:read_files", "table:session_permissions", "table:queue_tasks", "table:pending_injects", "table:session_run_queue", "table:orphan_call_outbox", "table:async_hosts", "table:session_drivers", "table:session_notices", "table:wake_schedules", "table:batch_nodes", "table:batch_control_requests", "table:batch_runner_checkpoints"} {
		require.Contains(t, fixtureObjects, tableKey)
	}
	require.Equal(t, embeddedObjects, fixtureObjects)
}

func normalizeTableColumnOrder(ddl string) string {
	open := strings.Index(ddl, " (")
	close := strings.LastIndex(ddl, ")")
	if open < 0 || close <= open {
		return ddl
	}
	body := ddl[open+2 : close]
	var parts []string
	start, depth := 0, 0
	var quote byte
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quote != 0 {
			if c == quote {
				if i+1 < len(body) && body[i+1] == quote {
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(body[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(body[start:]))
	var columns, constraints []string
	for _, part := range parts {
		if strings.HasPrefix(part, "constraint ") || strings.HasPrefix(part, "primary key ") || strings.HasPrefix(part, "unique ") || strings.HasPrefix(part, "foreign key ") || strings.HasPrefix(part, "check ") {
			constraints = append(constraints, part)
		} else {
			columns = append(columns, part)
		}
	}
	sort.Strings(columns)
	return ddl[:open+2] + strings.Join(append(columns, constraints...), ",") + ddl[close:]
}
