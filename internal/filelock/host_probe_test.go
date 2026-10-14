package filelock

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared-probe tests need a REAL second OS process to hold the
// EXCLUSIVE lock: within one process, flock/byte-range semantics make
// "another holder" indistinguishable from self. Pattern: re-exec the
// test binary with a sentinel env var (the classic
// TestHelperProcess idiom), child acquires via this package's own
// AcquireFileLock, reports LOCKED on stdout, then blocks on stdin until
// the parent closes it.

const helperProcessEnv = "RUSH_FILELOCK_HELPER_PROCESS"

// TestHelperFileLockHold is not a real test -- it is the re-exec entry
// point invoked via `-test.run=^TestHelperFileLockHold$`. Without the
// sentinel env var it does nothing and returns immediately.
//
// Protocol on stdout, one unbuffered line: "LOCKED" once the exclusive
// lock is held, "FAILED <error>" otherwise. Then the process blocks on
// os.Stdin.Read until the parent closes stdin.
func TestHelperFileLockHold(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		return
	}
	// Not really a *testing.T-driven test from here on; use os.Exit so
	// the exit code is meaningful to the parent.
	lockPath := os.Getenv("RUSH_FILELOCK_HELPER_LOCKPATH")
	if lockPath == "" {
		fmt.Printf("FAILED missing-env\n")
		os.Exit(2)
	}
	f, err := OpenLockFile(lockPath)
	if err != nil {
		os.Stdout.WriteString("FAILED open: " + err.Error() + "\n")
		os.Exit(1)
	}
	lk, err := AcquireFromFile(f, lockPath)
	if err != nil {
		os.Stdout.WriteString("FAILED lock: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("LOCKED\n")

	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	_ = lk.Release()
	os.Exit(0)
}

// spawnFileLockHolder starts a real child process that holds the
// EXCLUSIVE lock on lockPath, and blocks until the child reports LOCKED
// (bounded timeout -- no sleeps). Caller ends the hold via the returned
// release func (closing stdin), which also waits for exit.
func spawnFileLockHolder(t *testing.T, lockPath string) (release func()) {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := platform.Command(ctx, exe, "-test.run=^TestHelperFileLockHold$")
	cmd.Env = append(os.Environ(),
		helperProcessEnv+"=1",
		"RUSH_FILELOCK_HELPER_LOCKPATH="+lockPath,
	)

	stdinW, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdoutR, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = nil // drain nothing; we do not assert on it

	require.NoError(t, cmd.Start(), "start helper process")

	// First non-empty stdout line, with a bounded timeout so a broken
	// helper cannot hang the suite.
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				lineCh <- line
				return
			}
		}
		lineCh <- ""
	}()

	select {
	case line := <-lineCh:
		require.Equal(t, "LOCKED", line, "helper must report the lock held")
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timed out waiting for helper process to report LOCKED")
	}

	return func() {
		_ = stdinW.Close()
		_ = cmd.Wait()
	}
}

func TestProbeShared_MissingFileIsDead(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "never-existed.lock")

	status, err := ProbeShared(lockPath)
	require.NoError(t, err)
	assert.Equal(t, LockStatusDead, status)

	// The probe must not have created the file.
	_, statErr := os.Stat(lockPath)
	assert.True(t, os.IsNotExist(statErr), "probe must not create the lock file")
}

func TestProbeShared_UnlockedExistingFileIsDead(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "unlocked.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	status, err := ProbeShared(lockPath)
	require.NoError(t, err, "probe wins the shared lock: no exclusive holder exists")
	assert.Equal(t, LockStatusDead, status)

	// The won shared lock must be released: an exclusive acquire
	// succeeds immediately afterwards.
	lk, err := TryAcquireFileLock(lockPath)
	require.NoError(t, err, "probe must release the shared lock it won")
	require.NoError(t, lk.Release())
}

func TestProbeShared_ExclusiveHolderInOtherProcessIsAlive(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "held.lock")
	release := spawnFileLockHolder(t, lockPath)

	// Dead->Alive transition across processes: the same path probes Dead
	// once no one holds it (covered above) and Alive while a real
	// second process holds the exclusive lock.
	status, err := ProbeShared(lockPath)
	require.NoError(t, err)
	assert.Equal(t, LockStatusAlive, status)

	// A second probe, while the exclusive holder is still live, reports
	// the same Alive verdict: shared probes coexist with each other and
	// the holder is never disturbed.
	status, err = ProbeShared(lockPath)
	require.NoError(t, err)
	assert.Equal(t, LockStatusAlive, status)

	release()
}

func TestProbeShared_DirectoryIsUnknown(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "dir.lock")
	require.NoError(t, os.Mkdir(lockPath, 0o755))

	status, err := ProbeShared(lockPath)
	require.Error(t, err)
	assert.Equal(t, LockStatusUnknown, status)
}
