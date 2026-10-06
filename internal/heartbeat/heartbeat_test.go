package heartbeat

// Revert-check map (test -> production mechanism it catches):
//   TestPersistedStringsRedactedAndBounded        -> persistString + identity-keyed map & filename hash
//   TestCompactSnapshotAlwaysWithinBudget         -> foldSnapshotModels <=1 guard + hardTrim
//   TestWorkerStartedOnceAcrossRestart            -> workerOnce (no resumeRecording restart)
//   TestShutdownPublishesWithoutWorker            -> Shutdown does not depend on a started worker
//   TestFirstRecordFlushesWithoutManualForce      -> ensureWorker startup flush path
//   TestReaderRecoversSecondAttempt               -> readFile retry hook in ReadAll
//   TestHundredThousandRecordsNoRequestPathWrites -> ensureProcessMeta before Lock (no IO on hot path)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setup resets all package state and installs a temp heartbeat dir.
func setup(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", d)
	SetDirFunc(nil)
	registry.Lock()
	registry.rows = map[string]*record{}
	initWorkspace = ""
	initSlots = map[string]string{}
	processStart = ""
	processParent = ""
	processPID = 0
	processPPID = 0
	processMetaOnce = sync.Once{}
	processMetaHook = nil
	registry.Unlock()
	resetWorkerForTest()
	setNow(time.Now)
	setWriteFile(os.WriteFile)
	setRenameFile(os.Rename)
	setReadFile(os.ReadFile)
	setFlushForTest(nil)
	lastFlushMu.Lock()
	lastFlushAt = time.Time{}
	lastFlushMu.Unlock()
	t.Cleanup(func() {
		Shutdown(time.Second)
	})
	return d
}

// withCtx attaches a Context to the background.
func withCtx(c Context) context.Context { return WithContext(context.Background(), c) }

func TestRootFilesModelsAndPurpose(t *testing.T) {
	d := setup(t)
	RecordRequest(withCtx(Context{SessionID: "root", RootSessionID: "root", AgentID: "root", Purpose: PurposeTurn, Role: "smart", Source: "slot@start"}), "p", "m1", nil)
	RecordRequest(withCtx(Context{SessionID: "child", RootSessionID: "root", AgentID: "call-xyz", Purpose: PurposeTitle, Role: "worker", Source: "per-call"}), "p", "m2", nil)
	RecordRequest(withCtx(Context{SessionID: "root", Purpose: PurposeOther}), "p", "m3", nil)
	Shutdown(time.Second)
	fs, _ := os.ReadDir(d)
	if len(fs) != 1 {
		t.Fatalf("files=%v", fs)
	}
	es, e := ReadAll()
	if e != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, e)
	}
	if len(es[0].Models) != 3 || len(es[0].Agents) != 2 {
		t.Fatalf("models=%d agents=%d", len(es[0].Models), len(es[0].Agents))
	}
	if es[0].Models["p/m2"].ByPurpose["title"].Requests != 1 {
		t.Fatal("title purpose missing")
	}
	if es[0].Session != "root" {
		t.Fatal(es[0].Session)
	}
}

func TestIdentityNoDollarParsing(t *testing.T) {
	setup(t)
	// Old code stripped "$$...": now the full SessionID is the identity.
	RecordRequest(withCtx(Context{SessionID: "root$$task"}), "p", "m", nil)
	registry.Lock()
	_, ok := registry.rows["root$$task"]
	registry.Unlock()
	if !ok {
		t.Fatal("session suffix was parsed; must be verbatim")
	}
}

func TestSeparateRootsAndContext(t *testing.T) {
	setup(t)
	RecordRequest(withCtx(Context{SessionID: "w1$$call_ab", AgentID: "w1$$call_ab"}), "p", "m", nil)
	RecordRequest(withCtx(Context{SessionID: "w2"}), "p", "m", nil)
	Shutdown(time.Second)
	es, _ := ReadAll()
	if len(es) != 2 {
		t.Fatal(len(es))
	}
	if FromContext(context.Background()) != (Context{}) {
		t.Fatal("nonzero empty context")
	}
}

func TestFullIdentityNotMergedOnLongPrefix(t *testing.T) {
	setup(t)
	a := strings.Repeat("x", 200) + "a"
	b := strings.Repeat("x", 200) + "b"
	RecordRequest(withCtx(Context{RootSessionID: a}), "p", "m", nil)
	RecordRequest(withCtx(Context{RootSessionID: b}), "p", "m", nil)
	registry.Lock()
	defer registry.Unlock()
	if len(registry.rows) != 2 {
		t.Fatalf("long-prefix identities merged: %d", len(registry.rows))
	}
	if filename(launchCwd, a) == filename(launchCwd, b) {
		t.Fatal("filename hash collision on full identity")
	}
}

func TestCallerFieldBoundsAfterRedact(t *testing.T) {
	setup(t)
	secret := "ghp_123456789012345678901234"
	RecordRequest(withCtx(Context{SessionID: "s", AgentID: strings.Repeat("a", 250), Role: strings.Repeat("r", 150), Source: strings.Repeat("s", 150)}), strings.Repeat("p", 150), strings.Repeat("n", 150), errors.New("token="+secret))
	registry.Lock()
	defer registry.Unlock()
	r := registry.rows["s"]
	for _, m := range r.Models {
		if len([]rune(m.Provider)) > 100 || len([]rune(m.Model)) > 100 {
			t.Fatal("provider/model unbounded")
		}
		if strings.Contains(m.LastError, secret) {
			t.Fatal("secret persisted")
		}
	}
	if len([]rune(r.Agents[0].ID)) > 200 {
		t.Fatal("agent id unbounded")
	}
}

func TestRedaction(t *testing.T) {
	for _, s := range []string{
		"Bearer abc123", "api_key=secret", "api-key: secret", "sk-abcdef123",
		"https://user:pass@example.com/x?key=secret", `{"x-api-key":"secret"}`,
		`{"access_token": "abc"}`, "Basic abc123", "token=abc", "password=xyz",
		"AIza123456789012345678901234", "gsk_secret", "xai-secret",
		"ghp_123456789012345678901234", "glpat-secret", "ssh://u:p@host/path",
	} {
		r := RedactError(errors.New(s))
		if strings.Contains(r, "secret") || strings.Contains(r, "abc123") || strings.Contains(r, "abcdef123") || strings.Contains(r, "xyz") || strings.Contains(r, "u:p") {
			t.Fatalf("not redacted: %s", r)
		}
		if strings.Contains(r, "AIza123456789012345678901234") || strings.Contains(r, "ghp_123456789012345678901234") {
			t.Fatalf("key shape leaked: %s", r)
		}
	}
	if len([]rune(RedactError(errors.New(strings.Repeat("ü", 300))))) > 200 {
		t.Fatal("unbounded")
	}
}

func TestLimitClassification(t *testing.T) {
	for _, s := range []string{"HTTP 429", "status:429", "code = 429", "too many requests", "rate-limit", "rate limit", "quota", "usage limit", "limit reached"} {
		if !isLimit(s) {
			t.Fatalf("missed: %s", s)
		}
	}
	if isLimit("port 54293") {
		t.Fatal("false positive on port")
	}
}

func TestNoSecretsInSnapshot(t *testing.T) {
	d := setup(t)
	RecordRequest(context.Background(), "provider", "sk-secret", errors.New("Bearer supersecret api_key=secret"))
	Shutdown(time.Second)
	fs, _ := os.ReadDir(d)
	b, e := os.ReadFile(filepath.Join(d, fs[0].Name()))
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"supersecret", "secret", "sk-secret"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("leaked %s", s)
		}
	}
}

func TestConcurrentRecording(t *testing.T) {
	setup(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				RecordRequest(context.Background(), "p", "m", nil)
			}
		}()
	}
	wg.Wait()
	registry.Lock()
	defer registry.Unlock()
	if registry.rows["_none"].Totals.Requests != 10000 {
		t.Fatal(registry.rows["_none"].Totals.Requests)
	}
}

func TestMissingDir(t *testing.T) {
	SetDirFunc(nil)
	t.Setenv("RUSH_HEARTBEAT_DIR", filepath.Join(t.TempDir(), "missing"))
	es, e := ReadAll()
	if e != nil || len(es) != 0 {
		t.Fatalf("%v %v", es, e)
	}
	n, e := Prune(time.Hour)
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestFilenameAdversarial(t *testing.T) {
	for _, s := range []string{"A", "a", "a/b", "a_b", strings.Repeat(".", 500) + "id", strings.Repeat("d", 200)} {
		n := filename(strings.Repeat("x", 400), s)
		if len(n) > 120 || strings.HasPrefix(n, ".") {
			t.Fatalf("bad name %q for %q", n, s)
		}
	}
	// A vs a: same slug, but full-identity hash differs.
	if filename("dir", "A") == filename("dir", "a") {
		t.Fatal("case collision")
	}
	// a/b vs a_b: slug same, hash differs.
	if filename("dir", "a/b") == filename("dir", "a_b") {
		t.Fatal("slash collision")
	}
	// >40-char dirs: slug truncated, full dir hash differs.
	long1 := strings.Repeat("d", 41) + "1"
	long2 := strings.Repeat("d", 41) + "2"
	if filename(long1, "id") == filename(long2, "id") {
		t.Fatal("dir hash collision")
	}
	if !strings.Contains(filename("dir", "id"), fmt.Sprintf("--%d.json", os.Getpid())) {
		t.Fatal("pid suffix missing")
	}
}

func TestUsageFoldAndPurposeTotalsBothPaths(t *testing.T) {
	setup(t)
	for i := 0; i < 20; i++ {
		AddUsage(withCtx(Context{Purpose: PurposeTurn}), "p", fmt.Sprintf("m%d", i), Usage{Input: int64(i + 1)})
	}
	RecordRequest(withCtx(Context{Purpose: PurposeTurn}), "p", "m0", nil)
	registry.Lock()
	defer registry.Unlock()
	r := registry.rows["_none"]
	if len(r.Models) > maxModels {
		t.Fatalf("models=%d exceeds cap", len(r.Models))
	}
	if r.Totals.Input != 210 || r.Totals.ByPurpose["turn"].Input != 210 {
		t.Fatalf("totals=%+v", r.Totals)
	}
	if r.Totals.ByPurpose["turn"].Requests != 1 {
		t.Fatal("purpose requests not tracked on request path")
	}
	if _, ok := r.Models[otherKey]; !ok {
		t.Fatal("no folded (other) model")
	}
	var sumReq int64
	for _, m := range r.Models {
		sumReq += m.Requests
	}
	if sumReq != r.Totals.Requests {
		t.Fatalf("fold lost requests: %d vs %d", sumReq, r.Totals.Requests)
	}
}

func TestRevisitAfterEvictNotSilentlyZero(t *testing.T) {
	setup(t)
	// Give every evicted model real usage history so the fold must preserve it.
	for i := 0; i < maxModels; i++ {
		AddUsage(withCtx(Context{Purpose: PurposeTurn}), "p", fmt.Sprintf("m%d", i), Usage{Input: int64(i + 1)})
	}
	getModel(registryRow(), "p", "revisited") // triggers fold
	registry.Lock()
	defer registry.Unlock()
	r := registry.rows["_none"]
	other := r.Models[otherKey]
	if other == nil || other.Input != 136 { // sum 1..16
		t.Fatalf("(other).Input=%v", other)
	}
	if r.Totals.Input != 136 || r.Totals.ByPurpose["turn"].Input != 136 {
		t.Fatalf("totals.Input=%d purpose=%d", r.Totals.Input, r.Totals.ByPurpose["turn"].Input)
	}
	var sum int64
	for _, mm := range r.Models {
		sum += mm.Input
	}
	if sum != r.Totals.Input {
		t.Fatalf("fold lost input: %d vs %d", sum, r.Totals.Input)
	}
}

func registryRow() *record {
	registry.Lock()
	defer registry.Unlock()
	r := registry.rows["_none"]
	if r == nil {
		r = fresh(Context{})
		registry.rows[r.identity] = r
	}
	return r
}

// TestPersistedStringsRedactedAndBounded catches removal of persistString and
// of the identity-keyed rows map / full-identity filename hash.
func TestPersistedStringsRedactedAndBounded(t *testing.T) {
	d := setup(t)
	Init("ws api_key=zzz", map[string]string{"k": "v api_key=zzz"})
	long := strings.Repeat("x", 19900) + "token=abc"
	id1 := long[:299] + "a"
	id2 := long[:299] + "b"
	for _, id := range []string{id1, id2} {
		RecordRequest(withCtx(Context{
			RootSessionID: id, SessionID: long,
			AgentID: strings.Repeat("a", 250) + " Bearer abcdef",
			Role:    strings.Repeat("r", 150) + "token=secret",
			Source:  strings.Repeat("s", 150) + "password=x",
		}), "p", "m", nil)
	}
	Shutdown(time.Second)
	fs, e := os.ReadDir(d)
	if e != nil {
		t.Fatal(e)
	}
	if len(fs) < 2 {
		t.Fatalf("expected >=2 files, got %d", len(fs))
	}
	if filename(launchCwd, id1) == filename(launchCwd, id2) {
		t.Fatal("prefix-sharing ids produced one filename")
	}
	for _, f := range fs {
		b, err := os.ReadFile(filepath.Join(d, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 16384 {
			t.Fatalf("%s is %d bytes", f.Name(), len(b))
		}
		s := string(b)
		for _, secret := range []string{"token=abc", "token=secret", "Bearer abcdef", "password=x", "api_key=zzz"} {
			if strings.Contains(s, secret) {
				t.Fatalf("%s leaked %q", f.Name(), secret)
			}
		}
	}
}
