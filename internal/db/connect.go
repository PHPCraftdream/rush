package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
)

var (
	pragmas = map[string]string{
		"foreign_keys":  "ON",
		"journal_mode":  "WAL",
		"page_size":     "4096",
		"temp_store":    "MEMORY",
		"cache_size":    "-8000",
		"synchronous":   "NORMAL",
		"secure_delete": "ON",
		"busy_timeout":  "30000",
	}
)

//go:embed migrations/*.sql
var FS embed.FS

func init() {
	goose.SetBaseFS(FS)

	if testing.Testing() {
		goose.SetLogger(goose.NopLogger())
	}
}

// connEntry holds a shared database connection pair and its reference
// count. db is the single-connection writer (SetMaxOpenConns(1) — see the
// comment in Connect for why). readDB is a separate, read-only pool that
// WAL allows to run concurrently with the writer without touching the
// corruption-prone write path; it is only ever nil-safe-equal to db itself
// when opening a genuinely separate read-only handle isn't possible (see
// ConnectRead). Both handles share one refCount and are opened/closed
// together so Connect/ConnectRead/Release callers don't have to reason
// about two independent lifetimes.
type connEntry struct {
	db       *sql.DB
	readDB   *sql.DB
	refCount int
	pathLock *pathLock
}

// pathLock serializes database lifecycle operations for one path. refs holds
// one reference for every Connect/Release operation that has acquired the
// path lock, plus one while the path has a pooled connection. Keeping the
// pooled reference prevents a new operation from observing an old pathLock
// while another operation is still finishing with it.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

var (
	pool   = make(map[string]*connEntry)
	poolMu sync.Mutex

	// pathLocks contains only paths with an active opener, waiter, or pooled
	// connection. pathLocksMu protects both the map and each pathLock's refs;
	// the pathLock mutex itself serializes the slow open/close lifecycle for
	// one path. poolMu only guards pool bookkeeping, never slow database work.
	pathLocks   = make(map[string]*pathLock)
	pathLocksMu sync.Mutex

	// onOpenNew runs after a cache miss, before the slow open+migrate
	// work, for a database path not yet in the pool. Test seam only:
	// lets tests pause one path mid-open to prove unrelated paths
	// don't serialize behind it. Nil outside tests.
	onOpenNew func(absPath string)

	// onCloseEntry runs while the path lock is held, immediately before a
	// pooled entry is closed. Test seam only; nil outside tests.
	onCloseEntry func(absPath string)

	// onPathLockAcquired runs after a path-lock registry reference is acquired.
	// Test seam only; nil outside tests.
	onPathLockAcquired func(absPath string)
)

func acquirePathLock(absPath string) *pathLock {
	pathLocksMu.Lock()
	lock := pathLocks[absPath]
	if lock == nil {
		lock = &pathLock{}
		pathLocks[absPath] = lock
	}
	lock.refs++
	pathLocksMu.Unlock()
	if onPathLockAcquired != nil {
		onPathLockAcquired(absPath)
	}
	return lock
}

func retainPathLock(pathLock *pathLock) {
	pathLocksMu.Lock()
	pathLock.refs++
	pathLocksMu.Unlock()
}

func releasePathLock(absPath string, pathLock *pathLock) {
	pathLocksMu.Lock()
	pathLock.refs--
	if pathLock.refs == 0 && pathLocks[absPath] == pathLock {
		delete(pathLocks, absPath)
	}
	pathLocksMu.Unlock()
}

// Connect opens a SQLite database connection for the given data
// directory and runs migrations. If a connection to the same database
// file already exists, the existing connection is returned with its
// reference count incremented. Callers must pair each Connect with a
// [Release] when they no longer need the connection.
//
// Connect only ever returns the single-connection WRITER handle. Callers
// that also want a concurrent read-only handle for hot, standalone read
// paths (list/grep/call-tree queries that don't need read-your-own-write
// consistency with a subsequent write in the same call) should additionally
// call [ConnectRead] with the same dataDir — it shares this entry's
// refCount, so a single [Release] tears down both.
func Connect(ctx context.Context, dataDir string) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return entry.db, nil
}

// ConnectRead opens (or reuses) a read-only connection pool for the same
// database Connect would open, sharing its reference count. WAL mode lets
// readers run fully concurrently with the single writer connection instead
// of queuing behind it — this is the point of keeping the two separate.
//
// dataDir must have already been (or be about to be) passed to Connect;
// calling ConnectRead alone still opens the writer underneath (migrations
// must run before anything reads), it just returns the reader handle. Every
// call increments the shared refCount, so a caller that calls both Connect
// and ConnectRead for the same dataDir must call [Release] the same number
// of times it called either one (once per Connect/ConnectRead pair, not
// once per function).
func ConnectRead(ctx context.Context, dataDir string) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	return entry.readDB, nil
}

// connect is the shared implementation behind Connect and ConnectRead: it
// opens (or reuses) the pool entry for dataDir, running migrations exactly
// once, and increments the entry's refCount once per call — so callers that
// want BOTH a writer and a reader handle for the same dataDir must call
// [Release] twice (once per Connect/ConnectRead call they made), matching
// the existing "one Connect, one Release" contract extended to ConnectRead.
func connect(ctx context.Context, dataDir string) (*connEntry, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data.dir is not set")
	}

	dbPath := filepath.Join(dataDir, "rush.db")

	// Resolve to an absolute path so that different relative paths to
	// the same file share a single connection.
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	// Serialize lifecycle operations of THIS path only. poolMu is deliberately
	// NOT held across the open/ping/migrate work below: it used to be, and
	// one global lock made every unrelated dataDir in the process
	// queue up behind a single fresh database's full migration run —
	// under -race this serialized dozens of parallel tests' fresh
	// t.TempDir() databases badly enough to eat a whole package's
	// 10-minute timeout (task #637). Same-path callers still serialize
	// here, so the "one connection per database file" guarantee below
	// is unchanged.
	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()
	defer func() {
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
	}()

	poolMu.Lock()
	if entry, ok := pool[absPath]; ok {
		entry.refCount++
		poolMu.Unlock()
		return entry, nil
	}
	poolMu.Unlock()

	if onOpenNew != nil {
		onOpenNew(absPath)
	}

	conn, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// Serialize all access through a single connection. SQLite
	// serializes writes at the file level anyway, and allowing multiple
	// pool connections to interleave writes/checkpoints (especially
	// under concurrent sub-agents) has caused WAL/header desync
	// resulting in SQLITE_NOTADB (26) on the next open.
	conn.SetMaxOpenConns(1)

	if err = conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := Migrate(ctx, conn); err != nil {
		conn.Close()
		slog.Error("Failed to apply migrations", "error", err)
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	// Open the read-only pool AFTER migrations have run on the writer, on
	// the same absolute path. WAL mode (already set on the writer above,
	// and inherited file-wide) means these reader connections observe a
	// consistent snapshot without ever blocking on — or being blocked
	// by — the writer's single connection. mode=ro additionally guards
	// against a read path accidentally issuing a write: it will fail
	// fast with SQLITE_READONLY instead of silently taking the writer's
	// place.
	//
	// If the read-only pool fails to open for any reason, we don't fail
	// Connect/ConnectRead over it — we fall back to routing reads through
	// the writer connection (readDB = conn), which is exactly today's
	// behavior. A degraded-to-serialized read path is strictly better
	// than refusing to start.
	readConn, err := openReadDB(dbPath)
	if err != nil {
		slog.Warn("Failed to open read-only pool, reads will serialize with writes", "error", err)
		readConn = conn
	}

	// Keep the path lock registered for as long as this pool entry exists.
	// This reference is acquired before publishing the entry so a concurrent
	// Release cannot remove the registry entry between those two operations.
	retainPathLock(pathMu)
	entry := &connEntry{db: conn, readDB: readConn, refCount: 1, pathLock: pathMu}
	poolMu.Lock()
	pool[absPath] = entry
	poolMu.Unlock()
	return entry, nil
}

// Release decrements the reference count for the database at the given
// data directory. When the count reaches zero the underlying connections
// (writer and, if separate, reader) are closed and removed from the pool.
//
// A caller that obtained both a writer (Connect) and a reader (ConnectRead)
// handle for the same dataDir must call Release once per call it made to
// either function — the shared refCount is decremented once per Release,
// symmetric with connect()'s "increment once per call" contract.
func Release(dataDir string) error {
	dbPath := filepath.Join(dataDir, "rush.db")
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()

	poolMu.Lock()
	entry, ok := pool[absPath]
	if !ok {
		poolMu.Unlock()
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
		return nil
	}

	entry.refCount--
	if entry.refCount > 0 {
		poolMu.Unlock()
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
		return nil
	}

	// refCount reached zero: remove from pool while holding the mutex to
	// prevent concurrent Connect() from finding and incrementing it, then
	// close the actual connections while pathMu is held. A waiter may already
	// have acquired pathMu's registry reference; keeping the mutex until close
	// completes prevents it from opening a new generation while the old one is
	// still being torn down.
	delete(pool, absPath)
	poolMu.Unlock()

	if onCloseEntry != nil {
		onCloseEntry(absPath)
	}
	closeErr := closeEntry(entry)
	pathMu.mu.Unlock()
	releasePathLock(absPath, pathMu)
	releasePathLock(absPath, entry.pathLock)
	return closeErr
}

// ReleaseAll forcibly closes the pooled connection for dataDir and removes
// it from the pool, regardless of its current reference count. A no-op
// (returns nil) if no entry exists for dataDir, matching Release's own
// behavior in that case.
//
// Intended for tests that need to guarantee a specific dataDir's connection
// is fully closed at teardown, even when the number of paired Connect/
// ConnectRead calls a caller made isn't fully known or a cooperating
// caller's own cleanup may have left references outstanding on purpose.
// App.Shutdown's forced-shutdown path is exactly such a case: it
// deliberately skips calling Release at all (to avoid closing a DB out from
// under a still-active live writer in real CLI/server use, where the
// process exits immediately after and the OS reclaims the handle) — a test
// binary does NOT exit after one test, so relying on a single paired
// Release call in that scenario silently leaves the entry's refCount above
// zero forever, leaking the underlying file handle for the rest of the test
// binary's life. On Windows specifically, that leaked handle then makes
// t.TempDir()'s own RemoveAll cleanup fail (unlike POSIX, Windows will not
// delete a file that still has an open handle).
//
// Scoped to dataDir's own absolute path only — safe to call from a single
// test's cleanup even when other tests use the pool concurrently (e.g. via
// t.Parallel()) for DIFFERENT dataDirs, unlike ResetPool which tears down
// every pooled entry process-wide.
func ReleaseAll(dataDir string) error {
	dbPath := filepath.Join(dataDir, "rush.db")
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()

	poolMu.Lock()
	entry, ok := pool[absPath]
	if !ok {
		poolMu.Unlock()
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
		return nil
	}
	delete(pool, absPath)
	// Zero the refCount before unlocking (found by the sixth @oh review
	// pass): this entry is already unlinked from the pool, so nothing can
	// legitimately observe it again through Connect/Release — but zeroing
	// it defensively means a caller that (incorrectly) still held a
	// reference to this now-detached entry and later calls Release on it
	// cannot under-decrement a DIFFERENT, freshly-created entry for the
	// same dataDir path.
	entry.refCount = 0
	poolMu.Unlock()

	if onCloseEntry != nil {
		onCloseEntry(absPath)
	}
	closeErr := closeEntry(entry)
	pathMu.mu.Unlock()
	releasePathLock(absPath, pathMu)
	releasePathLock(absPath, entry.pathLock)
	return closeErr
}

// closeEntry closes both handles in entry, tolerating readDB == db (the
// fallback path in connect() when the read-only pool failed to open, or in
// principle any future caller that intentionally shares one *sql.DB between
// both roles) by only closing the reader when it is a distinct handle.
func closeEntry(entry *connEntry) error {
	var readErr error
	if entry.readDB != nil && entry.readDB != entry.db {
		readErr = entry.readDB.Close()
	}
	writeErr := entry.db.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// ResetPool closes all pooled connections and clears the pool. This is
// intended for use in tests to ensure a clean state between test cases.
func ResetPool() {
	type pooledEntry struct {
		path  string
		entry *connEntry
	}

	entries := make([]pooledEntry, 0)
	poolMu.Lock()
	for path, entry := range pool {
		delete(pool, path)
		entries = append(entries, pooledEntry{path: path, entry: entry})
	}
	poolMu.Unlock()

	for _, pooled := range entries {
		pooled.entry.pathLock.mu.Lock()
		if onCloseEntry != nil {
			onCloseEntry(pooled.path)
		}
		closeEntry(pooled.entry) //nolint:errcheck
		pooled.entry.pathLock.mu.Unlock()
		releasePathLock(pooled.path, pooled.entry.pathLock)
	}
}

// Migrate applies the embedded schema migrations to conn.
//
// Goose's legacy package-level API stores the selected dialect in a mutable
// global. That API is not safe when independent clients initialize databases
// concurrently. A Provider owns its dialect, filesystem, and migration
// operations, so each database gets an isolated migration boundary.
func Migrate(ctx context.Context, conn *sql.DB) error {
	options := make([]goose.ProviderOption, 0, 1)
	if testing.Testing() {
		options = append(options, goose.WithLogger(goose.NopLogger()))
	}
	migrationFS, err := fs.Sub(FS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to locate embedded migrations: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, migrationFS, options...)
	if err != nil {
		return fmt.Errorf("failed to initialize goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}
	return nil
}
