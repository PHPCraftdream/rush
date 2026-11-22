package cmdtest

// Shared session-test fixtures relocated (AST copies, names upper-cased)
// from internal/serve/cmd's root _test files: lock-file helpers, the
// sessions fixture DB opener, the config-env isolator and CaptureStdout.
// Package cmdtest exists solely for tests; the originals stay until the
// test-port workers delete them.

import (
	"bytes"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	"github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// CaptureStdout runs f while capturing os.Stdout output. Mirror of
// root sessions_locks_root_fixtures_test.go's captureStdout (and
// cmdtest.CaptureStderr): needed because some commands write their table
// (and --json output) to os.Stdout, not os.Stderr.
func CaptureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

// WriteLockFileAt writes a session lock file holding the given PID under
// <dataDir>/locks/ and returns its path. Copy of writeLockFileAt
// (sessions_why_descendant_test.go); the original stays until lane K3.
func WriteLockFileAt(t *testing.T, dataDir, sessionID string, pid int) string {
	t.Helper()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sessionfacts.SessionLockStem(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(pid)+"\n"), 0o644))
	return lockPath
}

// BackDateLock ages a lock file past LockStaleDuration so it reads as
// abandoned rather than merely quiet. Copy of backDateLock
// (sessions_why_descendant_test.go).
func BackDateLock(t *testing.T, path string) {
	t.Helper()
	stale := time.Now().Add(-(sessionlock.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(path, stale, stale))
}

// AgeLock back-dates a lock file's mtime by d. Display-only after D10:
// no reader decision may depend on it. Copy of ageLock
// (sessions_activity_cmd_test.go).
func AgeLock(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(path, old, old))
}

// ReleasedLock writes a released (empty, back-dated) lock file for
// sessionID under dataDir and returns its path. Copy of releasedLock
// (sessions_activity_cmd_test.go).
func ReleasedLock(t *testing.T, dataDir, sessionID string) string {
	t.Helper()
	path := WriteLockFileAt(t, dataDir, sessionID, 0)
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	BackDateLock(t, path)
	return path
}

// NewSessionFactsTestDB opens a fully migrated sqlite DB (db.Connect) on a
// temp dir and returns the connection plus its Queries. Copy of
// newSessionFactsTestDB (sessions_fixture_db_test.go).
func NewSessionFactsTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	return conn, db.New(conn)
}

// IsolateConfigEnvForTests points every global config/data env var at a
// throwaway temp tree, disables provider discovery, and silences the
// logger. Copy of isolateConfigEnvForTests (sessions_reset_test.go).
func IsolateConfigEnvForTests(t *testing.T) (tmp string) {
	t.Helper()
	config.ResetProviderCacheForTests()

	tmp = t.TempDir()

	dataDir := filepath.Join(tmp, "global-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)

	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	return tmp
}
