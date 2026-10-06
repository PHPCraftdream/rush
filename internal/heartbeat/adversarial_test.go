package heartbeat

// Revert-check map (test -> single production change it must catch):
//   TestPersistedIdentityRedactedAndBoundedOnDisk -> fresh: Session without persistString (unbounded id);
//                                                     updateRequest: AgentID without persistString (secret on disk)
//   TestPublishedRecordAlwaysWithinBudget          -> compactSnapshot: hardTrim not applied;
//                                                     foldSnapshotModels guard `<= 1` -> `== 0` (endless fold, nothing published)
//   TestProcessMetadataLookedUpOutsideRegistryLock -> ensureProcessMeta moved inside the registry lock
//   TestNewestAgentsKept                           -> updateRequest sort order (`>` -> `<` keeps the oldest agents)

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readPublished(t *testing.T, dir string) (all []byte, sizes []int) {
	t.Helper()
	fs, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
		sizes = append(sizes, len(b))
	}
	return all, sizes
}

func TestPersistedIdentityRedactedAndBoundedOnDisk(t *testing.T) {
	d := setup(t)
	RecordRequest(withCtx(Context{RootSessionID: "root token=SESSIONSECRET tail", AgentID: "Bearer AGENTSECRET", Role: "smart", Purpose: PurposeTurn}), "p", "m", nil)
	long := strings.Repeat("y", 20000)
	RecordRequest(withCtx(Context{RootSessionID: long}), "p", "m", nil)
	Shutdown(10 * time.Second)
	raw, sizes := readPublished(t, d)
	if len(sizes) != 2 {
		t.Fatalf("want 2 published files, got %d", len(sizes))
	}
	for _, secret := range []string{"SESSIONSECRET", "AGENTSECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s reached the disk", secret)
		}
	}
	for _, n := range sizes {
		if n > snapshotBudget {
			t.Fatalf("file of %d bytes exceeds the budget", n)
		}
	}
	es, err := ReadAll()
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	for _, e := range es {
		if n := len([]rune(e.Session)); n > 200 {
			t.Fatalf("persisted session has %d runes", n)
		}
	}
}

func TestPublishedRecordAlwaysWithinBudget(t *testing.T) {
	d := setup(t)
	const models, per = 12, 400
	for m := 0; m < models; m++ {
		for i := 0; i < per; i++ {
			RecordRequest(withCtx(Context{
				RootSessionID: "big",
				AgentID:       fmt.Sprintf("agent-%d-%d", m, i),
				Role:          fmt.Sprintf("role-%d-%04d-%s", m, i, strings.Repeat("r", 20)),
				Source:        fmt.Sprintf("src-%d-%04d-%s", m, i, strings.Repeat("s", 20)),
				Purpose:       PurposeTurn,
			}), "p", fmt.Sprintf("m%d", m), nil)
		}
	}
	Shutdown(20 * time.Second)
	_, sizes := readPublished(t, d)
	if len(sizes) != 1 {
		t.Fatalf("want one published file, got %d", len(sizes))
	}
	if sizes[0] > snapshotBudget {
		t.Fatalf("published %d bytes, budget %d", sizes[0], snapshotBudget)
	}
	es, err := ReadAll()
	if err != nil || len(es) != 1 {
		t.Fatalf("%v %v", es, err)
	}
	if got := es[0].Totals.Requests; got != models*per {
		t.Fatalf("totals lost: %d", got)
	}
}

func TestProcessMetadataLookedUpOutsideRegistryLock(t *testing.T) {
	setup(t)
	var calls, held atomic.Int32
	processMetaHook = func() {
		calls.Add(1)
		if !registry.TryLock() {
			held.Add(1)
			return
		}
		registry.Unlock()
	}
	t.Cleanup(func() { processMetaHook = nil })
	RecordRequest(context.Background(), "p", "m", nil)
	if calls.Load() != 1 || held.Load() != 0 {
		t.Fatalf("RecordRequest: lookups=%d, with the registry lock held=%d", calls.Load(), held.Load())
	}
	setup(t)
	processMetaHook = func() {
		calls.Add(1)
		if !registry.TryLock() {
			held.Add(1)
			return
		}
		registry.Unlock()
	}
	calls.Store(0)
	AddUsage(context.Background(), "p", "m", Usage{Input: 1})
	if calls.Load() != 1 || held.Load() != 0 {
		t.Fatalf("AddUsage: lookups=%d, with the registry lock held=%d", calls.Load(), held.Load())
	}
}

func TestNewestAgentsKept(t *testing.T) {
	setup(t)
	var ns atomic.Int64
	ns.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	setNow(func() time.Time { return time.Unix(0, ns.Add(int64(10*time.Second))).UTC() })
	t.Cleanup(func() { setNow(time.Now) })
	const total = 24
	for i := 0; i < total; i++ {
		RecordRequest(withCtx(Context{RootSessionID: "only", AgentID: fmt.Sprintf("agent-%02d", i)}), "p", "m", nil)
	}
	registry.Lock()
	defer registry.Unlock()
	r := registry.rows["only"]
	if r == nil || len(r.Agents) != maxAgents {
		t.Fatalf("agents kept: %v", r)
	}
	have := map[string]bool{}
	for _, a := range r.Agents {
		have[a.ID] = true
	}
	for i := total - maxAgents; i < total; i++ {
		if !have[fmt.Sprintf("agent-%02d", i)] {
			t.Fatalf("newest agent %02d was dropped", i)
		}
	}
}
