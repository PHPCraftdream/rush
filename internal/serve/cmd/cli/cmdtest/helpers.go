package cmdtest

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/dbtest"
)

// captureStderr runs f while capturing os.Stderr output.
func CaptureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

// captureStdoutAndStderr runs f while capturing both os.Stdout and
// os.Stderr, mirroring captureStdout/captureStderr individually. Needed
// here because the fix under test writes to BOTH streams in one invocation
// (the warning to stderr, the listing to stdout) and both must be asserted
// on from the same run.
func CaptureStdoutAndStderr(t *testing.T, f func()) (stdout string, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr

	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = outW
	os.Stderr = errW
	defer func() {
		os.Stdout = oldOut
		os.Stderr = oldErr
	}()

	var outBuf, errBuf bytes.Buffer
	doneOut := make(chan struct{})
	doneErr := make(chan struct{})
	go func() {
		_, _ = io.Copy(&outBuf, outR)
		close(doneOut)
	}()
	go func() {
		_, _ = io.Copy(&errBuf, errR)
		close(doneErr)
	}()

	f()
	_ = outW.Close()
	_ = errW.Close()
	<-doneOut
	<-doneErr
	return outBuf.String(), errBuf.String()
}

// seedAuditDir points RUSH_GLOBAL_DATA and RUSH_GLOBAL_CONFIG at
// throwaway directories so no test touches the real global config.
func SeedAuditDir(t *testing.T) string {
	t.Helper()

	dataDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	return dataDir
}

func AwaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func MustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// journalSnapshotRestore snapshots value+Changed of every flag in the sets
// and returns a restore func.
func JournalSnapshotRestore(t *testing.T, sets ...*pflag.FlagSet) func() {
	t.Helper()
	type savedFlag struct {
		f       *pflag.Flag
		value   string
		slice   []string
		changed bool
	}
	var saved []savedFlag
	for _, fs := range sets {
		fs.VisitAll(func(f *pflag.Flag) {
			entry := savedFlag{f: f, value: f.Value.String(), changed: f.Changed}
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				entry.slice = append([]string(nil), sv.GetSlice()...)
			}
			saved = append(saved, entry)
		})
	}
	return func() {
		for _, s := range saved {
			// Set("[]") on a slice flag would append the literal "[]"; Replace restores the list.
			if sv, ok := s.f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(s.slice)
			} else {
				_ = s.f.Value.Set(s.value)
			}
			s.f.Changed = s.changed
		}
	}
}

// ExitRecorder records os.Exit calls without exiting.
type ExitRecorder struct {
	mu    sync.Mutex
	codes []int
	First chan struct{}
	once  sync.Once
}

func NewExitRecorder() *ExitRecorder { return &ExitRecorder{First: make(chan struct{})} }

func (r *ExitRecorder) Exit(code int) {
	r.mu.Lock()
	r.codes = append(r.codes, code)
	r.mu.Unlock()
	r.once.Do(func() { close(r.First) })
}

func (r *ExitRecorder) Calls() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.codes...)
}

func NewTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// With modernc's SQLite driver, each pooled connection to ":memory:" is
	// a SEPARATE, independently-empty database — there is no shared backing
	// store across connections the way file-backed SQLite has. Without
	// capping the pool to a single connection, database/sql is free to open
	// a second connection under concurrent use (e.g. a caller holding a
	// transaction open across multiple statements, as sessions_fork_test.go
	// does), and that second connection sees none of the tables created
	// below, failing with "no such table". See the identical fix/comment in
	// internal/session/session_test.go's newTestDB.
	sqlDB.SetMaxOpenConns(1)
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
			deleted_todos TEXT NOT NULL DEFAULT '[]',
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
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			ended_reason TEXT NOT NULL DEFAULT '',
			budget_max_cost REAL NOT NULL DEFAULT 0,
			budget_max_tokens INTEGER NOT NULL DEFAULT 0,
			budget_timeout_sec INTEGER NOT NULL DEFAULT 0,
			parent_cost_accounted REAL NOT NULL DEFAULT 0,
			origin TEXT DEFAULT '' NOT NULL,
			cost_self REAL NOT NULL DEFAULT 0,
			cost_base REAL NOT NULL DEFAULT 0,
			cost_parent_id TEXT NOT NULL DEFAULT '',
			workspace_root TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT ''
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
			origin TEXT DEFAULT '' NOT NULL,
			checkpoint_generation INTEGER NOT NULL DEFAULT 0,
			notice_kind TEXT NOT NULL DEFAULT ''
		);

		CREATE INDEX idx_messages_session_id ON messages(session_id);

		CREATE TABLE pending_injects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			interrupt INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);

		-- Forking reads the durable credential marker row from the run queue.
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
	`)
	require.NoError(t, err)

	require.NoError(t, dbtest.ApplyFinalizationFencingSchema(context.Background(), sqlDB))

	return sqlDB, db.New(sqlDB)
}
