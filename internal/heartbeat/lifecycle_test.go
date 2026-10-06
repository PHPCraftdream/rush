package heartbeat

// Revert-check map (test -> production mechanism it catches):
//   TestEndSessionFinalFlushThenDrop -> identity-keyed rows map (EndSession id)
//   TestPruneNeverDeletesAliveSelfRealToken -> StartToken identity vs pid reuse

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/procinfo"
)

// requireStartTokens skips where procinfo has no process start token (BSD/macOS).
func requireStartTokens(t *testing.T) {
	t.Helper()
	if procinfo.StartToken(os.Getpid()) == "" {
		t.Skip("no process start token on this platform")
	}
}

func TestPruneNeverDeletesAliveSelfRealToken(t *testing.T) {
	d := setup(t)
	r := fresh(Context{SessionID: "live"})
	r.PID = os.Getpid()
	r.ProcStart = procinfo.StartToken(os.Getpid()) // real stored token for SELF
	r.LastBeat = now().Add(-48 * time.Hour).Format(time.RFC3339)
	b, _ := json.Marshal(r)
	p := filepath.Join(d, "live.json")
	os.WriteFile(p, b, 0o600)
	n, e := Prune(time.Hour)
	if e != nil || n != 0 {
		t.Fatalf("%d %v", n, e)
	}
	if _, e = os.Stat(p); e != nil {
		t.Fatal(e)
	}
}

func TestPruneNeverDeletesUnknownAlive(t *testing.T) {
	d := setup(t)
	// Stub the probe: alive but token unknown must never be pruned.
	orig := probeFn
	probeFn = func(pid int) (bool, string, bool) { return true, "", false }
	t.Cleanup(func() { probeFn = orig })
	r := fresh(Context{SessionID: "unknown"})
	r.PID = 12345
	r.ProcStart = ""
	r.LastBeat = now().Add(-48 * time.Hour).Format(time.RFC3339)
	b, _ := json.Marshal(r)
	p := filepath.Join(d, "unknown.json")
	os.WriteFile(p, b, 0o600)
	n, e := Prune(time.Hour)
	if e != nil || n != 0 {
		t.Fatalf("%d %v", n, e)
	}
	if _, e = os.Stat(p); e != nil {
		t.Fatal("unknown-alive entry was deleted")
	}
}

func TestPruneRemovesDeadStaleAndReused(t *testing.T) {
	requireStartTokens(t)
	d := setup(t)
	dead := fresh(Context{SessionID: "dead"})
	dead.PID = 2147483647
	dead.ProcStart = "bogus"
	dead.LastBeat = now().Add(-48 * time.Hour).Format(time.RFC3339)
	b, _ := json.Marshal(dead)
	os.WriteFile(filepath.Join(d, "dead.json"), b, 0o600)
	reused := fresh(Context{SessionID: "reused"})
	reused.PID = os.Getpid()
	reused.ProcStart = "wrong-token" // stored != real => pid reuse => stale
	reused.LastBeat = now().Add(-48 * time.Hour).Format(time.RFC3339)
	b, _ = json.Marshal(reused)
	os.WriteFile(filepath.Join(d, "reused.json"), b, 0o600)
	n, e := Prune(time.Hour)
	if e != nil || n != 2 {
		t.Fatalf("removed=%d err=%v", n, e)
	}
}

func TestPruneOnlyOwnTemps(t *testing.T) {
	d := setup(t)
	own := filepath.Join(d, ".ownfile--hash.json.json.tmp")
	other := filepath.Join(d, ".unrelated.tmp")
	bad := filepath.Join(d, "broken.json")
	for _, p := range []string{own, other, bad} {
		os.WriteFile(p, []byte("x"), 0o600)
		old := now().Add(-2 * time.Hour)
		os.Chtimes(p, old, old)
	}
	n, e := Prune(time.Hour)
	if e != nil || n != 2 {
		t.Fatalf("removed=%d err=%v", n, e)
	}
	if _, e = os.Stat(other); e != nil {
		t.Fatal("unrelated temp deleted")
	}
}

func TestReadAllStates(t *testing.T) {
	requireStartTokens(t)
	d := setup(t)
	// wrong token => stale
	r := fresh(Context{SessionID: "wrong"})
	r.PID = os.Getpid()
	r.ProcStart = "wrong-token"
	r.LastBeat = now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(d, "wrong.json"), b, 0o600)
	// torn file skipped
	os.WriteFile(filepath.Join(d, "broken.json"), []byte("{"), 0o600)
	// real record
	RecordRequest(context.Background(), "p", "m", nil)
	Shutdown(time.Second)
	es, e := ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	var found, wrong int
	for _, en := range es {
		if en.Session == "wrong" {
			wrong++
			// Process itself is alive (pid reused); state must be stale.
			if !en.Alive || en.State != "stale" {
				t.Fatalf("wrong token not stale: %+v", en)
			}
		}
		if en.Session == "_none" {
			found++
			if en.State != "stopped" {
				t.Fatalf("expected stopped, got %s", en.State)
			}
		}
	}
	if wrong != 1 || found != 1 {
		t.Fatalf("wrong=%d found=%d", wrong, found)
	}
}

func TestProcinfoTriStateSelfAndDeadChild(t *testing.T) {
	requireStartTokens(t)
	alive, token, known := procinfo.Probe(os.Getpid())
	if !alive || !known || token == "" {
		t.Fatalf("self: %v %q %v", alive, token, known)
	}
	alive, _, known = procinfo.Probe(2147483647)
	if alive || !known {
		t.Fatalf("dead child: %v %v", alive, known)
	}
}

func TestProcinfoThinWrappers(t *testing.T) {
	requireStartTokens(t)
	if !procinfo.Alive(os.Getpid()) {
		t.Fatal("self not alive")
	}
	if procinfo.StartToken(os.Getpid()) == "" {
		t.Fatal("self token empty")
	}
}

func TestEndSessionFinalFlushThenDrop(t *testing.T) {
	setup(t)
	RecordRequest(withCtx(Context{RootSessionID: "r"}), "p", "m", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	setFlushForTest(func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		flush(true)
	})
	done := make(chan struct{})
	go func() {
		EndSession("r")
		close(done)
	}()
	<-entered
	registry.Lock()
	_, present := registry.rows["r"]
	registry.Unlock()
	if !present {
		t.Fatal("row removed before final flush")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("EndSession blocked")
	}
	registry.Lock()
	_, present = registry.rows["r"]
	registry.Unlock()
	if present {
		t.Fatal("row retained after EndSession")
	}
	es, e := ReadAll()
	if e != nil || len(es) != 1 || es[0].State != "stopped" {
		t.Fatalf("%#v %v", es, e)
	}
}
