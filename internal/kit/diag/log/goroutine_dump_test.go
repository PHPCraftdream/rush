package log

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDumpGoroutines_WritesUsableDump is the regression guard for the reason
// this function exists at all: when a rush process hung in production there
// was no way to find out where. pprof is only served when RUSH_PROFILE is
// set (so it cannot be enabled after the fact on a live hang) and release
// builds strip symbols (so attaching a debugger returns nothing and kills the
// process). A dump written by the process itself, at the moment the hang is
// detected, is the only mechanism that survives all of that — so it must
// actually contain real stacks, not just an empty file.
func TestDumpGoroutines_WritesUsableDump(t *testing.T) {
	dir := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	path, err := DumpGoroutines("unit test")
	require.NoError(t, err)
	require.NotEmpty(t, path)
	assert.Equal(t, dir, filepath.Dir(path), "dump must land next to rush.log, not somewhere unrelated")

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(bts)

	assert.Contains(t, content, "reason: unit test", "the dump must record WHY it was taken")
	assert.Contains(t, content, "pid: "+strconv.Itoa(os.Getpid()),
		"the dump must name the process it came from — several rush processes share one .rush dir")
	assert.Contains(t, content, "goroutine ",
		"the dump must contain actual goroutine stacks, which is the entire point")
	assert.Contains(t, content, "DumpGoroutines",
		"the calling goroutine's own frame must be present, proving stacks are real and not truncated away")
}

// TestDumpGoroutines_FallsBackWhenLogDirUnset proves a dump is still produced
// before/without log.Setup — a hang during early startup is exactly when the
// operator has the least information, so this path must not silently no-op.
func TestDumpGoroutines_FallsBackWhenLogDirUnset(t *testing.T) {
	skipUnsetDirInTests = false
	t.Cleanup(func() { skipUnsetDirInTests = true })
	prev := logDir.Load()
	logDir.Store("")
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	path, err := DumpGoroutines("no log dir configured")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(path) })

	assert.True(t, strings.HasPrefix(filepath.Clean(path), filepath.Clean(os.TempDir())),
		"with no log dir configured the dump must fall back to the temp dir, got %s", path)
	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(bts), "goroutine ")
}

// TestCaptureGoroutineStack_DoesNoIO is the regression test for task #242:
// task #232 fixed onFire blocking cancel() on a hung os.WriteFile by
// dispatching the ENTIRE DumpGoroutines call (capture AND write) to an async
// goroutine. A later review found that went too far — it also deferred
// runtime.Stack's capture itself, so a turn that unwound quickly after
// cancel() could race the async goroutine and dump post-cancellation state
// instead of the actual hang, or (if the process exited first) never dump
// at all.
//
// The fix splits capture from write. This test proves CaptureGoroutineStack
// is the synchronous, I/O-free half: it must return a populated buffer
// immediately, without touching logDir, without creating any directory, and
// without writing any file — so it is safe to call directly inside a
// callback like onFire that must not block unboundedly.
func TestCaptureGoroutineStack_DoesNoIO(t *testing.T) {
	dir := t.TempDir()
	// Point logDir at a path that does not exist and cannot be created (by
	// deleting the temp dir itself), so any accidental I/O attempt inside
	// CaptureGoroutineStack would surface as a directory-not-found problem
	// if it tried to use logDir at all. CaptureGoroutineStack must not
	// consult logDir in the first place.
	missing := filepath.Join(dir, "does-not-exist", "nested")
	prev := logDir.Load()
	logDir.Store(missing)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	buf := CaptureGoroutineStack("io-free capture test")
	require.NotEmpty(t, buf, "capture must return a populated buffer")

	content := string(buf)
	assert.Contains(t, content, "reason: io-free capture test")
	assert.Contains(t, content, "pid: "+strconv.Itoa(os.Getpid()))
	assert.Contains(t, content, "goroutine ", "must contain real stacks")
	assert.Contains(t, content, "CaptureGoroutineStack",
		"the calling goroutine's own frame must be present, proving stacks are real and not truncated away")

	// The directory must genuinely not have been created — proof that
	// CaptureGoroutineStack performed no I/O of any kind.
	_, statErr := os.Stat(missing)
	assert.True(t, os.IsNotExist(statErr), "CaptureGoroutineStack must not create any directory — it must do zero I/O")
}

// TestWriteGoroutineDump_WritesCapturedBuffer proves WriteGoroutineDump — the
// half of the old DumpGoroutines that does the actual os.WriteFile — writes
// exactly the buffer it was handed, so splitting capture from write did not
// change what ends up on disk (bytes-for-bytes) versus the pre-split
// DumpGoroutines.
func TestWriteGoroutineDump_WritesCapturedBuffer(t *testing.T) {
	dir := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	buf := CaptureGoroutineStack("write test")
	path, err := WriteGoroutineDump(buf)
	require.NoError(t, err)
	require.NotEmpty(t, path)
	assert.Equal(t, dir, filepath.Dir(path))

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, buf, written, "WriteGoroutineDump must persist exactly the buffer CaptureGoroutineStack produced")
}

// TestCaptureGoroutineStack_ReturnsBeforeSlowWrite proves the concrete
// property the split exists for: capturing the stack can complete and be
// handed off to an async writer BEFORE that writer's (potentially slow or
// hung) os.WriteFile finishes — so a caller on a cancellation-critical path
// (agent.go's onFire) can safely capture synchronously and only defer the
// write, with no risk of the capture itself being delayed by disk I/O.
//
// This mirrors the shape of task #232's async dispatch tests (channel
// handshake, no time.Sleep guessing) but exercises the actual production
// helpers being split here rather than a synthetic closure.
func TestCaptureGoroutineStack_ReturnsBeforeSlowWrite(t *testing.T) {
	captureDone := make(chan struct{})
	writeEntered := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeDone := make(chan struct{})
	var writeFinished atomic.Bool

	buf := CaptureGoroutineStack("async write test")
	close(captureDone) // capture already returned by the time we get here

	go func() {
		close(writeEntered)
		<-releaseWrite // simulate a slow/hung disk write
		path, _ := WriteGoroutineDump(buf)
		if path != "" {
			_ = os.Remove(path)
		}
		writeFinished.Store(true)
		close(writeDone)
	}()

	select {
	case <-captureDone:
	default:
		t.Fatal("capture must have already completed synchronously")
	}

	select {
	case <-writeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the async write goroutine was never scheduled")
	}

	// The write is still blocked; capture's result (buf) was usable well
	// before this point, proving capture never waited on the write.
	assert.False(t, writeFinished.Load(), "write must still be pending — this proves capture did not wait for it")

	close(releaseWrite)

	// Must wait for the write to actually land before returning: it reads
	// the package-level logDir global, which the next test in this file
	// repoints at its own t.TempDir() as soon as it starts. An unawaited
	// write here can fire after that point and land inside the next test's
	// directory, producing a phantom extra file there (regression:
	// TestWriteGoroutineDump_ConcurrentDumpsDoNotCollide saw 21 files
	// instead of 20 for exactly this reason).
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the async write goroutine never finished")
	}
}

// TestWriteGoroutineDump_ConcurrentDumpsDoNotCollide is the regression test
// for task #275: the dump filename used to be built from PID + a timestamp
// with ONE-SECOND precision alone. Several wedged sub-agent turns inside one
// process can each fire the stream watchdog within the same second, and
// their dumps then land on the identical path — silently overwriting or
// interleaving the only diagnostic evidence available, precisely when it is
// most needed.
func TestWriteGoroutineDump_ConcurrentDumpsDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	const n = 20
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := CaptureGoroutineStack("concurrent dump test")
			path, err := WriteGoroutineDump(buf)
			require.NoError(t, err)
			paths[i] = path
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, p := range paths {
		require.NotEmpty(t, p)
		require.False(t, seen[p], "duplicate dump path %q — two concurrent dumps collided onto the same file", p)
		seen[p] = true
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, n, "every concurrent dump must have produced its own file on disk")
}

// TestWriteGoroutineDump_TestBinaryWithoutDirWritesNothing: a test binary
// with no log dir must not drop dumps into the shared temp dir.
// Revert check: removing the skipUnsetDirInTests branch in
// WriteGoroutineDumpNamed writes a file and returns a nil error.
func TestWriteGoroutineDump_TestBinaryWithoutDirWritesNothing(t *testing.T) {
	prev := logDir.Load()
	logDir.Store("")
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})
	path, err := WriteGoroutineDump([]byte("x"))
	require.ErrorIs(t, err, errNoDumpDirInTest)
	require.Empty(t, path)
}

// TestWriteGoroutineDumpNamed_ResolvesDirBeforeDelayHook is the
// regression test for the CI flake in internal/agent's
// TestTurnStallDumpFileModeAndPrefix (TempDir RemoveAll "directory not
// empty"). WriteGoroutineDumpNamed used to read the package-global
// logDir AFTER running the write-delay hook, so an async dump-write
// goroutine parked in a hook installed by one test resolved the
// directory at WAKE time — by which point a later test's
// SetLogDirForTest had repointed logDir at its own t.TempDir, and the
// write landed there, racing that test's TempDir RemoveAll ("directory
// not empty"). The fix resolves the directory BEFORE the hook.
//
// Revert check: reverting the hook/dir ordering in
// WriteGoroutineDumpNamed makes the dump land in dir2, so the
// require.Len(dir1) assertion below fails with 0 files in dir1.
func TestWriteGoroutineDumpNamed_ResolvesDirBeforeDelayHook(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir1)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	hookEntered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	// Leak-proof: unblocks the parked writer even if a fatal fires early.
	defer releaseOnce.Do(func() { close(release) })
	SetWriteDelayHookForTest(t, func() {
		close(hookEntered)
		<-release
	})

	done := make(chan error, 1)
	go func() {
		_, err := WriteGoroutineDump([]byte("parked-write test"))
		done <- err
	}()

	select {
	case <-hookEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the write goroutine never reached the delay hook")
	}

	// Simulate the NEXT test repointing the global while this write is
	// still parked in the hook — exactly what the victim test's
	// SetLogDirForTest call does in the flake.
	logDir.Store(dir2)
	releaseOnce.Do(func() { close(release) })

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the write goroutine never finished after release")
	}

	// The dump must be in dir1 — the directory current when
	// WriteGoroutineDumpNamed was CALLED — and must never have landed
	// in dir2, the directory some later test had installed by wake time.
	matches1, err := filepath.Glob(filepath.Join(dir1, "goroutines-*.txt"))
	require.NoError(t, err)
	require.Len(t, matches1, 1,
		"the dump must land in the directory that was current at call time, not at wake time")
	matches2, err := filepath.Glob(filepath.Join(dir2, "goroutines-*.txt"))
	require.NoError(t, err)
	assert.Empty(t, matches2,
		"a write parked in the delay hook must never follow logDir into a later test's directory")
}

// TestPfkWaitForInflightDumpsBlocksUntilWriteFinishes proves the drain
// used by SetLogDirForTest actually waits for an in-flight write (one
// parked in the delay hook is still counted, because the in-flight
// counter is incremented before the hook) and returns once it
// completes — the property that makes SetLogDirForTest safe to call
// while a previous test's dump write is still pending.
func TestPfkWaitForInflightDumpsBlocksUntilWriteFinishes(t *testing.T) {
	dir := t.TempDir()
	prev := logDir.Load()
	logDir.Store(dir)
	t.Cleanup(func() {
		if s, ok := prev.(string); ok {
			logDir.Store(s)
		} else {
			logDir.Store("")
		}
	})

	hookEntered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	// Leak-proof: unblocks the parked writer even if a fatal fires early.
	defer releaseOnce.Do(func() { close(release) })
	SetWriteDelayHookForTest(t, func() {
		close(hookEntered)
		<-release
	})

	done := make(chan error, 1)
	go func() {
		_, err := WriteGoroutineDump([]byte("drain test"))
		done <- err
	}()
	select {
	case <-hookEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the write goroutine never reached the delay hook")
	}

	drained := make(chan bool, 1)
	go func() {
		drained <- pfkWaitForInflightDumps()
	}()

	// While the write is parked in the hook the drain must NOT report
	// completion. This is a non-completion check, so a short bounded
	// wait is enough to make it meaningful.
	select {
	case got := <-drained:
		t.Fatalf("pfkWaitForInflightDumps returned %v while a write was still parked in the hook", got)
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-drained:
		assert.True(t, got, "the drain must complete once the parked write finishes")
	case <-time.After(2 * time.Second):
		t.Fatal("pfkWaitForInflightDumps never returned after the write finished")
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the write goroutine never finished after release")
	}
}
