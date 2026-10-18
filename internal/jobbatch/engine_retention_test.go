package jobbatch

import (
	"fmt"
	"testing"
)

// Walk-set guard: catches removal of the retirement filter at the slice
// level - without it the live scheduling slice keeps every settled root.
func TestCoreSequentialRootsKeepLiveWalkSetEmpty(t *testing.T) {
	e := coreTestEngine(t, 1)
	const n = 64
	for i := 0; i < n; i++ {
		id := ID(fmt.Sprintf("R%d", i))
		coreTestSubmit(t, e, id, coreTestBatch(ModeParallel, 0, coreTestTask("")))
		if len(e.live) != 1 {
			t.Fatalf("live scheduling slice %d after submit %s", len(e.live), id)
		}
		coreTestFinish(t, e, id+".1", StateCompleted)
		if len(e.live) != 0 {
			t.Fatalf("settled root %s left in the live scheduling slice (%d entries)", id, len(e.live))
		}
	}
}

// Separate visit-count guard: with terminal roots retained in the live walk
// set, the settlement, notice and settled-view walks grow linearly with the
// settled-root count - roughly 11*i visits at root i (three forest walks of
// length i per Apply plus the admission scan), about 11*n*(n+1)/2 total,
// ~23k for n=64, while correct retirement costs about 11 per root (~700).
// The 20*n bound therefore fails through growth; the non-zero assertion
// fails first for a broken instrumentation.
func TestCoreWalkVisitsBoundedAcrossRoots(t *testing.T) {
	e := coreTestEngine(t, 1)
	const n = 64
	visits := map[string]int{}
	e.walkInstrument = func(source string, roots int) { visits[source] += roots }
	for i := 0; i < n; i++ {
		id := ID(fmt.Sprintf("R%d", i))
		coreTestSubmit(t, e, id, coreTestBatch(ModeParallel, 0, coreTestTask("")))
		coreTestFinish(t, e, id+".1", StateCompleted)
	}
	total := 0
	for _, count := range visits {
		total += count
	}
	if total == 0 {
		t.Fatal("walk instrumentation counted nothing")
	}
	if total > 20*n {
		t.Fatalf("walk visits %d across %d sequential roots: %v", total, n, visits)
	}
}

// Preservation guard, not a regression test: it also passes on the
// unmodified engine and pins the retained-root API against future
// retirement bugs.
func TestCoreRetainedRootAnswersArePreserved(t *testing.T) {
	e := coreTestEngine(t, 2)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestFinish(t, e, "A.1", StateCompleted)
	if v, ok := e.View("A"); !ok || !v.State.Terminal() {
		t.Fatalf("settled root view lost: %v %v", v, ok)
	}
	if v, ok := e.View("A.1"); !ok || !v.State.Terminal() {
		t.Fatalf("settled leaf view lost: %v %v", v, ok)
	}
	s, ok := e.Snapshot("A")
	if !ok || len(s.Descendants) != 1 {
		t.Fatalf("settled root snapshot lost: %v %v", s, ok)
	}
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	if v, ok := e.View("A"); !ok || !v.State.Terminal() {
		t.Fatalf("retained root disturbed by later applies: %v %v", v, ok)
	}
	if s2, ok := e.Snapshot("A"); !ok || s2.Counts != s.Counts || len(s2.Descendants) != 1 {
		t.Fatalf("retained root snapshot changed: %v %v", s2, ok)
	}
	coreTestApply(t, e, Event{Type: EventRelease, Root: "A"})
	if _, ok := e.View("A"); ok {
		t.Fatal("released root still viewable")
	}
	coreTestReject(t, e, Event{Type: EventRelease, Root: "A"}, ErrReleased)
	coreTestReject(t, e, Event{Type: EventStopRequested, Node: "A.1"}, ErrReleased)
	coreTestReject(t, e, Event{Type: EventStopRequested, Node: "A.9"}, ErrReleased)
	coreTestReject(t, e, Event{Type: EventSubmit, Root: "A", Spec: coreTestBatch(ModeParallel, 0)}, ErrDuplicateRoot)
	coreTestReject(t, e, Event{Type: EventStopRequested, Node: "ZZ.1"}, ErrUnknownNode)
	if v, ok := e.View("B.1"); !ok || v.State != StateStarting {
		t.Fatalf("unrelated root disturbed by release: %v %v", v, ok)
	}
}

// Catches a retirement that drops the cursor translation: after the first
// root retires, the next admission must start at the surviving root the
// unretired engine would have picked. Fails on untranslated-cursor
// retirement, which restarts the scan at the shifted slice head.
func TestCoreRoundRobinOrderSurvivesRetirement(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	if got := coreTestFinish(t, e, "A.1", StateCompleted).Commands[0].Node; got != "B.1" {
		t.Fatalf("first admission %q, want B.1", got)
	}
	if got := coreTestFinish(t, e, "B.1", StateCompleted).Commands[0].Node; got != "C.1" {
		t.Fatalf("admission after retirement %q, want C.1", got)
	}
	if got := coreTestFinish(t, e, "C.1", StateCompleted).Commands[0].Node; got != "B.2" {
		t.Fatalf("wrap-around admission %q, want B.2", got)
	}
	if got := coreTestFinish(t, e, "B.2", StateCompleted).Commands[0].Node; got != "C.2" {
		t.Fatalf("tail admission %q, want C.2", got)
	}
	coreTestFinish(t, e, "C.2", StateCompleted)
	if len(e.live) != 0 {
		t.Fatalf("live scheduling slice not empty at the end: %d", len(e.live))
	}
	if len(e.roots) != 3 {
		t.Fatalf("retained root list changed: %d", len(e.roots))
	}
}

// Cursor-translation edges. Derivations cite the ORIGINAL admission order
// (HEAD admission.go: scan starts at cursor over the full unshrunk root
// list; cursor = index+1 after each admission). Every finished leaf below
// was admitted by an earlier command, so every Started/Settled event
// carries a real token.
func TestCoreRetirementCursorEdges(t *testing.T) {
	// Edge 1, retired before the cursor: A(1 task), B(2), C(2), cap 1.
	// Submit A admits A.1 (index 0, cursor 1). Finishing A.1 admits B.1
	// (scan 1%3=1, cursor 2) and retires A (index 0 < 2); the original
	// engine scans index 2 next, so finishing B.1 must admit C.1.
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	if got := coreTestFinish(t, e, "A.1", StateCompleted).Commands[0].Node; got != "B.1" {
		t.Fatalf("edge 1 first admission %q, want B.1", got)
	}
	if got := coreTestFinish(t, e, "B.1", StateCompleted).Commands[0].Node; got != "C.1" {
		t.Fatalf("edge 1 admission after retirement %q, want C.1", got)
	}
	// The original cursor is now 3: finishing C.1 scans 3%3=0, skips the
	// terminal A root and the running B.1, and admits B.2.
	if got := coreTestFinish(t, e, "C.1", StateCompleted).Commands[0].Node; got != "B.2" {
		t.Fatalf("edge 1 wrap-around admission %q, want B.2", got)
	}
	// The original cursor after B.2 is 2: finishing B.2 settles B and scans
	// 2%3=2, admitting C.2.
	if got := coreTestFinish(t, e, "B.2", StateCompleted).Commands[0].Node; got != "C.2" {
		t.Fatalf("edge 1 tail admission %q, want C.2", got)
	}
	coreTestFinish(t, e, "C.2", StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 3 {
		t.Fatalf("edge 1 live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 2, retired at the cursor: A(1), B(1), cap 1. Finishing A.1 admits
	// B.1 (scan 1%2=1) and retires A; the original engine scans 2%2=0, skips
	// terminal A and finds nothing new. The translated live [B] cursor 1
	// behaves the same. Pins the translation; passes unmodified too.
	e = coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	if got := coreTestFinish(t, e, "A.1", StateCompleted).Commands[0].Node; got != "B.1" {
		t.Fatalf("edge 2 admission %q, want B.1", got)
	}
	if tr := coreTestFinish(t, e, "B.1", StateCompleted); len(tr.Commands) != 0 {
		t.Fatalf("edge 2 unexpected admission %v", tr.Commands)
	}
	if len(e.live) != 0 || len(e.roots) != 2 {
		t.Fatalf("edge 2 live %d roots %d", len(e.live), len(e.roots))
	}

	// Edge 3, retired after the cursor: A..D one task each, cap 1. Finishing
	// A.1 admits B.1 (scan 1%4=1) and retires A, leaving live [B,C,D] with
	// cursor 1. Stopping the still-pending D settles it at index 2 - behind
	// the cursor - and that removal must not shift the scan. The original
	// engine, scanning from cursor 2 over [A,B,C,D], admits C.1 when B.1
	// finishes; the translated live [B,C] cursor 1 scans 1%2=1 and admits
	// C.1 as well.
	e = coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "D", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	if got := coreTestFinish(t, e, "A.1", StateCompleted).Commands[0].Node; got != "B.1" {
		t.Fatalf("edge 3 first admission %q, want B.1", got)
	}
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "D"})
	if len(e.live) != 2 || len(e.roots) != 4 {
		t.Fatalf("edge 3 live %d roots %d after D stopped", len(e.live), len(e.roots))
	}
	if v, ok := e.View("D.1"); !ok || !v.State.Terminal() || v.Attempts != 0 {
		t.Fatalf("edge 3 stopped leaf %+v %v", v, ok)
	}
	if got := coreTestFinish(t, e, "B.1", StateCompleted).Commands[0].Node; got != "C.1" {
		t.Fatalf("edge 3 admission after D retired %q, want C.1", got)
	}
	coreTestFinish(t, e, "C.1", StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 4 {
		t.Fatalf("edge 3 live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 4, several roots retiring in one event: with cap 1 only A.1 is
	// admitted (cursor 1); B and C are all-pending. HostClosing settles and
	// retires both B and C in a single Apply while A stays live cancelling.
	// The original engine's next scan from cursor 1 skips terminal B and C
	// and wraps to A; the translated live [A] cursor 1%1=0 is the same.
	// Reachable through the public Apply API, so the edge is kept.
	e = coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	tr := coreTestApply(t, e, Event{Type: EventHostClosing})
	if len(tr.Commands) != 1 || tr.Commands[0].Type != CommandStopLeaf || tr.Commands[0].Node != "A.1" {
		t.Fatalf("edge 4 closing commands %v", tr.Commands)
	}
	if len(e.live) != 1 || len(e.roots) != 3 {
		t.Fatalf("edge 4 live %d roots %d after the multi-retirement event", len(e.live), len(e.roots))
	}
	if _, ok := e.View("B"); !ok || !e.nodes["B"].view.State.Terminal() {
		t.Fatal("edge 4 retired root B lost or not terminal")
	}
	tr = coreTestFinish(t, e, "A.1", StateCompleted)
	if !tr.Final || !e.final {
		t.Fatalf("edge 4 final not reached: %+v", tr)
	}
	if len(e.live) != 0 || len(e.roots) != 3 {
		t.Fatalf("edge 4 live %d roots %d after final", len(e.live), len(e.roots))
	}
}
