package heartbeat

// Revert-check map (test -> production mechanism it catches):
//   TestQuiesceForTest_WaitsForInFlightFlushAndStopsWorker -> flushMu serialisation in QuiesceForTest + worker stop/done channels

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQuiesceForTest_WaitsForInFlightFlushAndStopsWorker proves QuiesceForTest
// waits for an in-flight flush and leaves the worker stopped and the dir empty.
func TestQuiesceForTest_WaitsForInFlightFlushAndStopsWorker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", dir)
	SetDirFunc(nil)
	lastFlushMu.Lock()
	lastFlushAt = time.Time{}
	lastFlushMu.Unlock()
	setNow(time.Now)
	setRenameFile(os.Rename)
	setReadFile(os.ReadFile)
	setFlushForTest(nil)
	t.Cleanup(func() {
		setWriteFile(os.WriteFile)
		setNow(time.Now)
		QuiesceForTest()
	})

	// Block the first write inside writeFile; count and locate every write.
	entered := make(chan struct{})
	gate := make(chan struct{})
	var writes int
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		if filepath.Dir(p) != dir {
			t.Errorf("write outside heartbeat dir: %s", p)
		}
		writes++
		if writes == 1 {
			close(entered)
			<-gate
		}
		return os.WriteFile(p, b, m)
	})

	AddUsage(WithContext(context.Background(), Context{SessionID: "s1", Purpose: PurposeTurn}), "prov", "m", Usage{Input: 1})
	flushDone := make(chan struct{})
	go func() {
		flush(true)
		close(flushDone)
	}()
	<-entered

	quiesced := make(chan struct{})
	workerCtrl.mu.Lock()
	workerDone := workerCtrl.done
	workerCtrl.mu.Unlock()
	go func() {
		QuiesceForTest()
		close(quiesced)
	}()
	select {
	case <-quiesced:
		t.Fatal("QuiesceForTest returned while a flush was in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate)
	select {
	case <-flushDone:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not finish after gate opened")
	}
	select {
	case <-quiesced:
	case <-time.After(2 * time.Second):
		t.Fatal("QuiesceForTest did not return after flush finished")
	}

	// Worker stopped and reset.
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker goroutine did not exit after QuiesceForTest")
	}
	workerCtrl.mu.Lock()
	stopped := workerCtrl.stop == nil && workerCtrl.done == nil
	workerCtrl.mu.Unlock()
	if !stopped || workerStarts.Load() != 0 {
		t.Fatal("worker not stopped/reset after QuiesceForTest")
	}

	// Registry cleared: a forced flush must not write anything or add files.
	before, _ := os.ReadDir(dir)
	requestFlush(true)
	if writes != 1 {
		t.Fatalf("post-quiesce flush wrote %d extra times", writes-1)
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(after) != len(before) {
		t.Fatalf("post-quiesce flush changed dir: %v -> %v (%v)", before, after, err)
	}

	// The package still works after a quiesce.
	AddUsage(WithContext(context.Background(), Context{SessionID: "s2", Purpose: PurposeTurn}), "prov", "m", Usage{Input: 1})
	FlushForTest()
	if writes != 2 {
		t.Fatalf("expected exactly one post-quiesce snapshot write, got %d total", writes)
	}
}
