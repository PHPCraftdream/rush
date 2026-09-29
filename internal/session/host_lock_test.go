package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// setupHostLockDB opens a real, migrated SQLite DB for host-lock tests that
// touch async_hosts/async_jobs (doc sec.3.6 requires real-DB coverage, not
// a fake).
func setupHostLockDB(t *testing.T) (context.Context, *db.Queries) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	return ctx, db.New(conn)
}

// failingStore's RegisterAsyncHost always fails -- used to prove
// RegisterHost releases the lock it already won before returning
// the DB error (doc sec.3.6: a failed registration must never leak a held
// lock).
type failingStore struct{}

func (failingStore) RegisterAsyncHost(context.Context, db.RegisterAsyncHostParams) (db.AsyncHost, error) {
	return db.AsyncHost{}, errors.New("boom: db insert failed")
}
func (failingStore) DeleteAsyncHostIfNoJobs(context.Context, string) (int64, error) { return 0, nil }

// spyStore records whether RegisterAsyncHost was ever called -- used to
// prove a lock-acquisition failure never reaches the DB at all.
type spyStore struct{ called bool }

func (s *spyStore) RegisterAsyncHost(context.Context, db.RegisterAsyncHostParams) (db.AsyncHost, error) {
	s.called = true
	return db.AsyncHost{}, nil
}
func (s *spyStore) DeleteAsyncHostIfNoJobs(context.Context, string) (int64, error) { return 0, nil }

func TestRegisterHost_LazyRegistration(t *testing.T) {
	ctx, q := setupHostLockDB(t)
	dataDir := t.TempDir()

	h, err := RegisterHost(ctx, dataDir, 4242, "cli", q)
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.NotEmpty(t, h.ID)
	assert.True(t, IsOwnHostID(h.ID), "the registering process must mark its own host id")

	row, err := q.GetAsyncHost(ctx, h.ID)
	require.NoError(t, err, "RegisterAsyncHost must have inserted a display-only row")
	assert.EqualValues(t, 4242, row.Pid)
	assert.Equal(t, "cli", row.Label)

	// The lock is genuinely held: a second attempt to lock the exact same
	// path (a different open file description, same process -- flock/
	// LockFileEx both correctly conflict here) must contend.
	_, err = TryAcquireFileLock(HostLockPath(dataDir, h.ID))
	require.Error(t, err)
	var contended *ErrLockContended
	assert.True(t, errors.As(err, &contended))

	require.NoError(t, h.Close(ctx, q))
	unmarkOwnHostID(h.ID) // test cleanup; Close already does this in production
}

func TestRegisterHost_DBFailureReleasesLock(t *testing.T) {
	dataDir := t.TempDir()

	_, err := RegisterHost(context.Background(), dataDir, 1, "cli", failingStore{})
	require.Error(t, err)

	entries, err := os.ReadDir(HostsDir(dataDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the lock file is created before the DB insert is attempted")

	lockPath := filepath.Join(HostsDir(dataDir), entries[0].Name())
	lock, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err, "a failed registration must release its lock, not leak it")
	require.NoError(t, lock.Release())
}

func TestRegisterHost_FailsWhenLockCannotBeAcquired(t *testing.T) {
	base := t.TempDir()
	// dataDir names a PLAIN FILE, not a directory: HostLockPath's parent
	// (dataDir/hosts) can never be created under it, so
	// TryAcquireFileLock's os.MkdirAll fails deterministically -- the
	// general "the lock file cannot even be created" case doc sec.3.6
	// requires registration to fail outright for (the literal NFS-without-
	// flock scenario is not portably reproducible in a test, but the
	// contract -- acquisition failure means registration failure, and the
	// DB is never touched -- is the same).
	dataDir := filepath.Join(base, "not-a-dir")
	require.NoError(t, os.WriteFile(dataDir, []byte("x"), 0o644))

	spy := &spyStore{}
	_, err := RegisterHost(context.Background(), dataDir, 1, "cli", spy)
	require.Error(t, err)
	assert.False(t, spy.called, "the DB row must never be inserted when the lock could not be acquired")
}

func TestProbeHostLock_DeadViaENOENT(t *testing.T) {
	dataDir := t.TempDir()
	status, lock, err := ProbeHostLock(HostLockPath(dataDir, "never-existed"))
	require.NoError(t, err)
	assert.Equal(t, HostStatusDead, status)
	assert.Nil(t, lock)
}

func TestProbeHostLock_DeadViaWonLock(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "host-a")

	// Create the file, then release -- it exists on disk but nobody holds it.
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	status, lock, err := ProbeHostLock(lockPath)
	require.NoError(t, err)
	assert.Equal(t, HostStatusDead, status)
	require.NotNil(t, lock, "winning the lock on an existing-but-unheld file must return the won lock")
	require.NoError(t, lock.Release())
}

func TestProbeHostLock_Alive(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "host-b")

	holder, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	status, lock, err := ProbeHostLock(lockPath)
	require.NoError(t, err)
	assert.Equal(t, HostStatusAlive, status)
	assert.Nil(t, lock)
}

func TestProbeHostLock_Unknown(t *testing.T) {
	dataDir := t.TempDir()
	// A path that exists but is a DIRECTORY, not a lock file: opening it
	// O_RDWR fails with a non-ENOENT, non-contention error on every
	// platform this repo supports (EISDIR on POSIX; access denied on
	// Windows) -- exactly the "some other error" case doc sec.3.6 requires
	// to classify as unknown, never dead.
	lockPath := HostLockPath(dataDir, "host-c")
	require.NoError(t, os.MkdirAll(lockPath, 0o755))

	status, lock, err := ProbeHostLock(lockPath)
	require.Error(t, err)
	assert.Equal(t, HostStatusUnknown, status)
	assert.Nil(t, lock)
}

func TestProbeHost_RefusesOwnHostID(t *testing.T) {
	ctx, q := setupHostLockDB(t)
	dataDir := t.TempDir()

	h, err := RegisterHost(ctx, dataDir, 1, "cli", q)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close(ctx, q) })

	status, lock, err := ProbeHost(dataDir, h.ID)
	assert.Equal(t, HostStatusUnknown, status)
	assert.Nil(t, lock)
	assert.ErrorIs(t, err, ErrProbeOwnHost)
}

func TestHostIdentity_CloseDeletesFileWhenNoRows(t *testing.T) {
	ctx, q := setupHostLockDB(t)
	dataDir := t.TempDir()

	h, err := RegisterHost(ctx, dataDir, 1, "cli", q)
	require.NoError(t, err)
	lockPath := HostLockPath(dataDir, h.ID)

	require.NoError(t, h.Close(ctx, q))

	rows, err := q.ListAsyncHosts(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		assert.NotEqual(t, h.ID, r.ID, "a host with zero referencing jobs must have its row deleted at Close")
	}

	_, statErr := os.Stat(lockPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "Close must delete the lock file when no rows remain")
	assert.False(t, IsOwnHostID(h.ID), "Close must un-mark the host id")
}

func TestHostIdentity_CloseKeepsFileWhenRowsExist(t *testing.T) {
	ctx, q := setupHostLockDB(t)
	dataDir := t.TempDir()

	h, err := RegisterHost(ctx, dataDir, 1, "cli", q)
	require.NoError(t, err)
	lockPath := HostLockPath(dataDir, h.ID)

	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "call-1", Kind: "command",
		InputHash: "h", HostID: h.ID, CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)

	require.NoError(t, h.Close(ctx, q))

	_, err = q.GetAsyncHost(ctx, h.ID)
	require.NoError(t, err, "a host row with referencing jobs must survive Close")

	_, statErr := os.Stat(lockPath)
	require.NoError(t, statErr, "the lock file must survive Close when rows remain")

	// The OS lock itself must still be released (only the file/row survive).
	status, lock, err := ProbeHostLock(lockPath)
	require.NoError(t, err)
	assert.Equal(t, HostStatusDead, status, "Close must release the OS lock even when the row/file survive")
	if lock != nil {
		require.NoError(t, lock.Release())
	}
}

func TestRemoveDeadHostFile_DeletesAfterIdentityVerified(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "dead-host")

	// Simulate a crashed host: acquire then release without deleting --
	// same on-disk state a killed process leaves (the lock file exists,
	// nobody holds it).
	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	status, lock, err := ProbeHostLock(lockPath)
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, status)
	require.NotNil(t, lock)

	require.NoError(t, RemoveDeadHostFile(lockPath, lock))

	_, statErr := os.Stat(lockPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "a verified-dead host's file must be removed")
}

func TestRemoveDeadHostFile_LosesIdentityRaceSafely(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "dead-host-2")
	otherPath := HostLockPath(dataDir, "some-other-host")

	// "held" stands in for the lock this process won at probe time, but by
	// the time RemoveDeadHostFile runs, lockPath's inode has changed under
	// it -- modeled here by pointing held at a DIFFERENT real file rather
	// than reproducing OS-specific delete-while-open timing: on Windows,
	// deleting/recreating lockPath while this process still holds it open
	// is impossible without FILE_SHARE_DELETE (confirmed empirically: it
	// fails the test's own os.Remove with a sharing violation), so a
	// same-process repro of the literal race is platform-dependent. The
	// SameFile comparison RemoveDeadHostFile performs is exercised exactly
	// the same way either way: held's fd identity vs. a fresh stat of
	// lockPath.
	held, err := TryAcquireFileLock(otherPath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(lockPath, []byte("new owner"), 0o644))

	require.NoError(t, RemoveDeadHostFile(lockPath, held), "losing the identity race must not be reported as an error")

	content, err := os.ReadFile(lockPath)
	require.NoError(t, err, "the NEW file at the path must survive -- RemoveDeadHostFile must not delete it")
	assert.Equal(t, "new owner", string(content))
}

func TestProbeHostLockShared_AliveDoesNotDisturbHolder(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "host-shared-alive")

	holder, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)

	status, err := ProbeHostLockShared(lockPath)
	require.NoError(t, err)
	assert.Equal(t, HostStatusAlive, status)

	// The probe must not have disturbed the holder: a fresh exclusive
	// attempt while the holder is still live must still contend exactly as
	// it would have before the probe ran.
	_, err = TryAcquireFileLock(lockPath)
	require.Error(t, err)
	var contended *ErrLockContended
	assert.True(t, errors.As(err, &contended))

	// And the holder itself can still release cleanly afterwards.
	require.NoError(t, holder.Release())
}

func TestProbeHostLockShared_DeadReleasedLockIsReusable(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "host-shared-dead")

	seed, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	status, err := ProbeHostLockShared(lockPath)
	require.NoError(t, err)
	assert.Equal(t, HostStatusDead, status)

	// The shared probe must have released its own lock before returning --
	// an exclusive acquire must succeed immediately, with no lock handed
	// back from the probe to get in the way.
	lock, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err, "ProbeHostLockShared must release the shared lock it won before returning")
	require.NoError(t, lock.Release())
}

func TestProbeHostLockShared_DeadViaENOENT(t *testing.T) {
	dataDir := t.TempDir()
	status, err := ProbeHostLockShared(HostLockPath(dataDir, "never-existed"))
	require.NoError(t, err)
	assert.Equal(t, HostStatusDead, status)
}

func TestProbeHostLockShared_Unknown(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := HostLockPath(dataDir, "host-shared-c")
	require.NoError(t, os.MkdirAll(lockPath, 0o755))

	status, err := ProbeHostLockShared(lockPath)
	require.Error(t, err)
	assert.Equal(t, HostStatusUnknown, status)
}

func TestProbeHostShared_RefusesOwnHostID(t *testing.T) {
	ctx, q := setupHostLockDB(t)
	dataDir := t.TempDir()

	h, err := RegisterHost(ctx, dataDir, 1, "cli", q)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close(ctx, q) })

	status, err := ProbeHostShared(dataDir, h.ID)
	assert.Equal(t, HostStatusUnknown, status)
	assert.ErrorIs(t, err, ErrProbeOwnHost)
}

// seedSession inserts a minimal sessions row so async_jobs.owner_session_id
// (FK, ON DELETE CASCADE) has a valid target.
func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
}
