package heartbeat

// Revert-check map (test -> production mechanism it catches):
//   TestCompactSnapshotAlwaysWithinBudget     -> foldSnapshotModels <=1 guard + hardTrim
//   TestWorkerStartedOnceAcrossRestart        -> workerOnce (no resumeRecording restart)
//   TestShutdownPublishesWithoutWorker        -> Shutdown without a started worker
//   TestFirstRecordFlushesWithoutManualForce  -> ensureWorker startup flush path
//   TestReaderRecoversSecondAttempt           -> readFile retry hook in ReadAll
//   TestHundredThousandRecordsNoRequestPathWrites -> ensureProcessMeta before Lock

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecordDoesNotPerformProcessIO(t *testing.T) {
	setup(t)
	ensureProcessMeta() // first call performs IO; later calls must not.
	var calls int32
	processMetaOnce = sync.Once{}
	processMetaHook = func() { atomic.AddInt32(&calls, 1) }
	ensureProcessMeta()
	atomic.StoreInt32(&calls, 0)
	for i := 0; i < 1000; i++ {
		RecordRequest(context.Background(), "p", "m", nil)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("metadata provider called %d times on hot path", calls)
	}
}

func TestHundredThousandRecordsNoRequestPathWrites(t *testing.T) {
	setup(t)
	setFlushForTest(func() {}) // suppress worker-driven flush entirely
	t.Cleanup(func() { setFlushForTest(nil) })
	var mu sync.Mutex
	writes := 0
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		mu.Lock()
		writes++
		mu.Unlock()
		return os.WriteFile(p, b, m)
	})
	for i := 0; i < 100000; i++ {
		RecordRequest(context.Background(), "p", "m", nil)
	}
	mu.Lock()
	defer mu.Unlock()
	if writes != 0 {
		t.Fatalf("request path wrote %d times", writes)
	}
	registry.Lock()
	r := registry.rows["_none"]
	if r == nil || r.Totals.Requests != 100000 {
		registry.Unlock()
		t.Fatal("records lost")
	}
	r.dirty = false
	registry.Unlock()
}

func TestFlushThrottleBoundsWrites(t *testing.T) {
	setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	var ns atomic.Int64
	ns.Store(time.Now().UnixNano())
	setNow(func() time.Time { return time.Unix(0, ns.Load()) })
	lastFlushMu.Lock()
	lastFlushAt = time.Time{}
	lastFlushMu.Unlock()
	var mu sync.Mutex
	writes := 0
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		mu.Lock()
		writes++
		mu.Unlock()
		return os.WriteFile(p, b, m)
	})
	for i := 0; i < 20; i++ {
		registry.Lock()
		r := registry.rows["_none"]
		r.dirty = true
		r.generation++
		registry.Unlock()
		flush(false)
		ns.Add(int64(time.Second))
	}
	mu.Lock()
	defer mu.Unlock()
	if writes > 5 {
		t.Fatalf("throttle allowed %d writes", writes)
	}
	t.Cleanup(func() { setNow(time.Now) })
}

// TestFirstRecordFlushesWithoutManualForce catches a broken ensureWorker
// startup-flush path: a single RecordRequest must produce a write.
func TestFirstRecordFlushesWithoutManualForce(t *testing.T) {
	setup(t)
	wrote := make(chan struct{}, 1)
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		select {
		case wrote <- struct{}{}:
		default:
		}
		return os.WriteFile(p, b, m)
	})
	RecordRequest(context.Background(), "p", "m", nil)
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot write after RecordRequest")
	}
}

func TestConcurrentShutdownSingleCompletion(t *testing.T) {
	d := setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Shutdown(time.Second)
		}()
	}
	wg.Wait()
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatalf("files=%v", fs)
	}
	b, err := os.ReadFile(filepath.Join(d, fs[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var got record
	if json.Unmarshal(b, &got) != nil || got.State != "stopped" {
		t.Fatalf("flushed state=%q", got.State)
	}
	registry.Lock()
	defer registry.Unlock()
	if registry.rows["_none"].State != "stopped" {
		t.Fatal("row not stopped")
	}
}

func TestShutdownWaitsBlockingFinalFlush(t *testing.T) {
	d := setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return os.WriteFile(p, b, m)
	})
	done := make(chan struct{})
	go func() {
		Shutdown(time.Second)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("final flush never started")
	}
	select {
	case <-done:
		t.Fatal("shutdown returned while flush in flight")
	default:
	}
	close(release)
	// Final flush may still be draining the mutex after the timer fired;
	// wait for the write to land.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fs, _ := os.ReadDir(d)
		if len(fs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("final write missing: %v", fs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShutdownTimeoutBoundsBlockingFinalFlush(t *testing.T) {
	setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return os.WriteFile(p, b, m)
	})
	start := time.Now()
	Shutdown(50 * time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("timeout did not bound shutdown")
	}
	close(release)
}

func TestRestartAfterShutdown(t *testing.T) {
	setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	Shutdown(time.Second)
	// Shutdown before any record must not poison later recording.
	RecordRequest(context.Background(), "p", "m", nil)
	registry.Lock()
	r := registry.rows["_none"]
	if r == nil || r.State != "running" {
		registry.Unlock()
		t.Fatal("restart failed")
	}
	registry.Unlock()
	Shutdown(time.Second)
}

func TestUnwritableDirThenRecoveredFileInPlace(t *testing.T) {
	d := setup(t)
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	RecordRequest(context.Background(), "p", "m", nil)
	Shutdown(time.Second)
	registry.Lock()
	if len(registry.rows) != 1 {
		registry.Unlock()
		t.Fatal("row dropped on write failure")
	}
	registry.rows["_none"].dirty = true
	registry.rows["_none"].generation++
	registry.Unlock()
	if err := os.Remove(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	flush(true)
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatalf("retry did not publish: %v", fs)
	}
	var got record
	b, _ := os.ReadFile(filepath.Join(d, fs[0].Name()))
	if json.Unmarshal(b, &got) != nil || got.Totals.Requests != 1 {
		t.Fatalf("bad retry data: %s", b)
	}
}

// TestReaderRecoversSecondAttempt catches removal of the readFile retry hook:
// the first read is torn, the second (inside ReadAll) is valid.
func TestReaderRecoversSecondAttempt(t *testing.T) {
	d := setup(t)
	ensureProcessMeta() // fresh() no longer performs process IO
	r := fresh(Context{SessionID: "r"})
	b, _ := json.Marshal(r)
	path := filepath.Join(d, "snapshot.json")
	os.WriteFile(path, []byte("{"), 0o600) // torn
	calls := 0
	setReadFile(func(p string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("{"), nil // still torn on first attempt
		}
		return os.ReadFile(p)
	})
	t.Cleanup(func() { setReadFile(os.ReadFile) })
	os.WriteFile(path, b, 0o600) // valid snapshot on disk before ReadAll
	es, e := ReadAll()
	if e != nil || len(es) != 1 {
		t.Fatalf("reader recovery failed: %v %v", es, e)
	}
}

func TestFlushSnapshotNeverMutatesLiveRow(t *testing.T) {
	d := setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		registry.Lock()
		registry.rows["_none"].Totals.Requests += 7
		registry.rows["_none"].generation++
		registry.rows["_none"].dirty = true
		registry.Unlock()
		return os.WriteFile(p, b, m)
	})
	requestFlush(true)
	registry.Lock()
	got := registry.rows["_none"].Totals.Requests
	registry.Unlock()
	if got != 8 {
		t.Fatal(got)
	}
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatal(fs)
	}
}

func TestMetadataAndVersion(t *testing.T) {
	d := setup(t)
	Init("/workspace", map[string]string{"smart": "s", "worker": "w"})
	RecordRequest(context.Background(), "p", "m", nil)
	Shutdown(time.Second)
	fs, _ := os.ReadDir(d)
	b, e := os.ReadFile(filepath.Join(d, fs[0].Name()))
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m["v"] != float64(1) || m["workspace"] != "/workspace" || m["slots_at_start"] == nil {
		t.Fatalf("metadata: %s", b)
	}
}

func TestUsageAndSummary(t *testing.T) {
	setup(t)
	AddUsage(context.Background(), "p", "m", Usage{Input: 7, Output: 3, CostUSD: .5})
	registry.Lock()
	r := registry.rows["_none"]
	e := Entry{Models: r.Models, Alive: true}
	registry.Unlock()
	s := Summarize([]Entry{e})
	if len(s) != 1 || s[0].Model != "p/m" || s[0].Input != 7 || s[0].Live != 1 {
		t.Fatalf("summary: %#v", s)
	}
}

// TestCompactSnapshotAlwaysWithinBudget catches foldSnapshotModels returning
// true on an already-folded map and a missing hardTrim final stage.
func TestCompactSnapshotAlwaysWithinBudget(t *testing.T) {
	setup(t)
	id := strings.Repeat("y", 20000)
	r := fresh(Context{SessionID: id})
	r.identity = id
	for i := 0; i < 100; i++ {
		m := getModel(r, "p", fmt.Sprintf("m%d", i))
		m.Input = int64(i)
		if i == 0 {
			m.LastError = strings.Repeat("e", 200)
		}
	}
	m0 := getModel(r, "p", "m0")
	for i := 0; i < 200; i++ {
		addUnique(&m0.Roles, strings.Repeat("role", 25)+strconv.Itoa(i))
	}
	for i := 0; i < 100; i++ {
		r.Agents = append(r.Agents, &agent{ID: fmt.Sprintf("agent-%03d", i)})
	}
	r.Totals.Requests = 1000
	done := make(chan []byte, 1)
	go func() { done <- compactSnapshot(r) }()
	var b []byte
	select {
	case b = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("compactSnapshot deadlocked")
	}
	if len(b) > snapshotBudget {
		t.Fatalf("snapshot=%d exceeds budget %d", len(b), snapshotBudget)
	}
	var got record
	if json.Unmarshal(b, &got) != nil {
		t.Fatal("unmarshal failed")
	}
	if got.Totals == nil || got.Totals.Requests != 1000 {
		t.Fatalf("totals lost: %+v", got.Totals)
	}
}

// TestWorkerStartedOnceAcrossRestart catches a resumeRecording-style worker
// restart after Shutdown.
func TestWorkerStartedOnceAcrossRestart(t *testing.T) {
	d := setup(t)
	RecordRequest(context.Background(), "p", "m", nil)
	deadline := time.Now().Add(5 * time.Second)
	for workerStarts.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("worker never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	blocked := make(chan struct{})
	release := make(chan struct{})
	setWriteFile(func(p string, b []byte, m os.FileMode) error {
		select {
		case <-blocked:
		default:
			close(blocked)
		}
		<-release
		return os.WriteFile(p, b, m)
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		setWriteFile(os.WriteFile)
	})
	go Shutdown(100 * time.Millisecond)
	<-blocked
	RecordRequest(context.Background(), "p", "m", nil)
	registry.Lock()
	starts := workerStarts.Load()
	st := registry.rows["_none"].State
	registry.Unlock()
	if starts != 1 {
		t.Fatalf("worker restarted: starts=%d", starts)
	}
	if st != "running" {
		t.Fatalf("state=%q", st)
	}
	close(release)
	requestFlush(true)
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatalf("files=%v", fs)
	}
}

// setupDir returns the heartbeat dir configured by setup.
func setupDir() string {
	return os.Getenv("RUSH_HEARTBEAT_DIR")
}

// TestShutdownPublishesWithoutWorker catches Shutdown depending on a started
// worker: a manually inserted row must still publish state "stopped".
func TestShutdownPublishesWithoutWorker(t *testing.T) {
	d := setup(t)
	registry.Lock()
	r := fresh(Context{SessionID: "noWorker"})
	registry.rows[r.identity] = r
	registry.Unlock()
	Shutdown(time.Second)
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatalf("files=%v", fs)
	}
	b, err := os.ReadFile(filepath.Join(d, fs[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var got record
	if json.Unmarshal(b, &got) != nil || got.State != "stopped" {
		t.Fatalf("state=%q body=%s", got.State, b)
	}
	registry.Lock()
	dirty := registry.rows[r.identity].dirty
	registry.Unlock()
	if dirty {
		t.Fatal("row still dirty")
	}
}
