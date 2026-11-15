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
// settled-root count - roughly 10*i+1 visits at root i (three forest walks
// of length i per Apply plus the admission scan), about 5*n*n in total,
// ~20k for n=64, while correct retirement costs about 11 per root (~700).
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

// finishAdmitted finishes the leaf named by a Start command returned by a
// previous Apply. The node id and token come from that command, so a wrong
// admission order surfaces as a failed lookup instead of being papered over
// by state-derived tokens.
func finishAdmitted(t *testing.T, e *Engine, cmd Command, state State) Transition {
	t.Helper()
	if cmd.Type != CommandStartLeaf {
		t.Fatalf("not a start command: %+v", cmd)
	}
	if n := e.nodes[cmd.Node]; n == nil || !n.accepted {
		coreTestApply(t, e, Event{Type: EventStarted, Node: cmd.Node, Token: cmd.Token})
	}
	return coreTestApply(t, e, Event{Type: EventSettled, Node: cmd.Node, Token: cmd.Token, Result: Result{State: state}})
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
// unretired engine would have picked. Every finished leaf below is derived
// from the previous Apply's Start command. Original admission (HEAD
// admission.go): submit A admits A.1 (index 0, cursor 1); finishing A.1
// admits B.1 (scan 1%3=1, cursor 2) and retires A (index 0 < 2); the
// original engine scans index 2 next and admits C.1, while an untranslated
// cursor restarts at the shifted head and admits B.2.
func TestCoreRoundRobinOrderSurvivesRetirement(t *testing.T) {
	e := coreTestEngine(t, 1)
	submitA := coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	if len(submitA.Commands) != 1 || submitA.Commands[0].Node != "A.1" {
		t.Fatalf("submit A commands %v", submitA.Commands)
	}
	tr := finishAdmitted(t, e, submitA.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.1" {
		t.Fatalf("first admission %v, want B.1", tr.Commands)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.1" {
		t.Fatalf("admission after retirement %v, want C.1", tr.Commands)
	}
	// The original cursor is now 3: finishing C.1 scans 3%3=0, skips the
	// terminal A root and the running B.1, and admits B.2.
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.2" {
		t.Fatalf("wrap-around admission %v, want B.2", tr.Commands)
	}
	// The original cursor after B.2 is 2: finishing B.2 settles B and scans
	// 2%3=2, admitting C.2.
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.2" {
		t.Fatalf("tail admission %v, want C.2", tr.Commands)
	}
	finishAdmitted(t, e, tr.Commands[0], StateCompleted)
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
// is derived from the previous Apply's Start command, so every
// Started/Settled event carries a real node id and token.
func TestCoreRetirementCursorEdges(t *testing.T) {
	// Edge 1, retired before the cursor: A(1 task), B(2), C(2), cap 1.
	// Original: submit A admits A.1 (index 0, cursor 1); finishing A.1
	// admits B.1 (scan 1%3=1, cursor 2) and retires A (index 0 < 2). The
	// original engine scans index 2 next, so finishing B.1 must admit C.1;
	// an untranslated cursor restarts at the shifted head (2%2=0) and
	// admits B.2 instead.
	e := coreTestEngine(t, 1)
	submitA := coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	tr := finishAdmitted(t, e, submitA.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.1" {
		t.Fatalf("edge 1 first admission %v, want B.1", tr.Commands)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.1" {
		t.Fatalf("edge 1 admission after retirement %v, want C.1", tr.Commands)
	}
	// The original cursor is now 3: finishing C.1 scans 3%3=0, skips the
	// terminal A root and the running B.1, and admits B.2.
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.2" {
		t.Fatalf("edge 1 wrap-around admission %v, want B.2", tr.Commands)
	}
	// The original cursor after B.2 is 2: finishing B.2 settles B and scans
	// 2%3=2, admitting C.2.
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.2" {
		t.Fatalf("edge 1 tail admission %v, want C.2", tr.Commands)
	}
	finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 3 {
		t.Fatalf("edge 1 live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 2, retired immediately before the cursor with two further live
	// roots holding pending tasks: A(g), B(1 plain task), C(g), D(g), cap 2,
	// ByGroup g:1. Original: submit A admits A.1 (index 0, cursor 1), submit
	// B admits B.1 (index 1, cursor 2); C and D stay pending because group g
	// is full. Stopping and settling the running B.1 settles B; no admission
	// fires (C and D are group-blocked), the cursor stays 2, and retiring B
	// - the entry immediately before that cursor - leaves live [A,C,D]
	// translated to cursor 1. Finishing A.1 frees group g; the original
	// engine scans index 2 and admits C.1, and the translated cursor
	// 1%3=1 admits C.1 as well. An untranslated cursor 2 (2%3=2) or an
	// off-by-one kept+1 would admit D.1 and fail.
	e, err := NewEngine(Limits{MaxConcurrent: 2, ByGroup: map[string]int{"g": 1}, MaxDepth: 8, MaxNodes: 100})
	if err != nil {
		t.Fatal(err)
	}
	gTask := func() Spec {
		return coreTestBatch(ModeParallel, 0, Spec{Task: &TaskSpec{Kind: "controlled", Group: "g"}})
	}
	submitA = coreTestSubmit(t, e, "A", gTask())
	submitB := coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", gTask())
	coreTestSubmit(t, e, "D", gTask())
	if len(submitA.Commands) != 1 || len(submitB.Commands) != 1 {
		t.Fatalf("edge 2 submit commands %v / %v", submitA.Commands, submitB.Commands)
	}
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: submitB.Commands[0].Node})
	stopB := submitB.Commands[0]
	coreTestApply(t, e, Event{Type: EventControlReported, Node: stopB.Node, Token: stopB.Token, Op: ControlOpStop})
	finishAdmitted(t, e, stopB, StateCancelled)
	if len(e.live) != 3 || len(e.roots) != 4 {
		t.Fatalf("edge 2 live %d roots %d after B retired", len(e.live), len(e.roots))
	}
	tr = finishAdmitted(t, e, submitA.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.1" {
		t.Fatalf("edge 2 admission %v, want C.1 (an untranslated or off-by-one cursor would pick D.1)", tr.Commands)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 4 {
		t.Fatalf("edge 2 live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 2b, a mid-list retirement with three blocked survivors around
	// the translated cursor: A, B, C, D one plain task each, cap 1.
	// ORIGINAL (pre-change) engine, for comparison: admission scans the
	// unshrunk e.roots [A,B,C,D] and nothing is ever removed from it, so
	// after the same events the cursor is still 2 and the decisive
	// admission scans from index 2: C is eligible there, so the original
	// admits C.1 as well. The expectation below (C.1) therefore equals the
	// original behaviour, derived independently of the translation code.
	//
	// Current engine (admission scans e.live; retirement happens at the end
	// of the Apply, cursor translated to the number of survivors before the
	// old cursor), step by step:
	//   submit A          -> A.1 admitted at index 0, cursor 1, active 1.
	//   submit B, C, D    -> pending (capacity), cursor stays 1.
	//   reject A.1        -> A.1 pending, blocked[""] = gen 0; admit from
	//                        cursor 1 finds every leaf blocked, cursor 1.
	//   capacity signal   -> gen[""] = 1, unblocks; admit from 1%4=1
	//                        admits B.1 (cursor 2, active 1 = cap).
	//   reject B.1        -> B.1 pending, authorized 1 == gen 1, so
	//                        blocked[""] = 1; admit from cursor 2 finds C,
	//                        D, A and B blocked, cursor stays 2.
	//   stop B            -> pending B.1 cancels to terminal, B settles;
	//                        no admission (all blocked); B retires:
	//                        live [A,C,D], translated cursor = survivors
	//                        before 2 = 1.
	//   capacity signal   -> gen[""] = 2 unblocks. The decisive admission
	//                        has room for exactly one leaf (cap 1): from
	//                        the translated cursor 1%3=1 it admits C.1.
	//                        kept-1 (0) admits A.1; kept+1 (2) and an
	//                        untranslated cursor (2%3=2) both admit D.1.
	//                        Every wrong candidate therefore fails; kept+1
	//                        and an untranslated cursor are indistinguishable
	//                        from each other (both pick D.1); kept-1 is
	//                        distinct.
	e = coreTestEngine(t, 1)
	submitA = coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "D", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventStartRejected, Node: submitA.Commands[0].Node, Token: submitA.Commands[0].Token, Reason: RejectCapacity})
	tr = coreTestApply(t, e, Event{Type: EventCapacityAvailable, Group: ""})
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.1" {
		t.Fatalf("edge 2b setup admission %v, want B.1", tr.Commands)
	}
	coreTestApply(t, e, Event{Type: EventStartRejected, Node: tr.Commands[0].Node, Token: tr.Commands[0].Token, Reason: RejectCapacity})
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "B"})
	tr = coreTestApply(t, e, Event{Type: EventCapacityAvailable, Group: ""})
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.1" {
		t.Fatalf("edge 2b admission %v, want C.1 (kept-1 would pick A.1, kept+1 and an untranslated cursor D.1)", tr.Commands)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "D.1" {
		t.Fatalf("edge 2b follow-up admission %v, want D.1", tr.Commands)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "A.1" {
		t.Fatalf("edge 2b tail admission %v, want A.1", tr.Commands)
	}
	finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 4 {
		t.Fatalf("edge 2b live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 3, retired after the cursor: A..D one task each, cap 1. Original:
	// submit A admits A.1 (index 0, cursor 1); finishing A.1 admits B.1
	// (scan 1%4=1) and retires A, leaving the translated live [B,C,D] at
	// cursor 1. Stopping the still-pending D settles it at index 2 - behind
	// the cursor - and that removal must not shift the scan: the original
	// engine scans from cursor 2 and admits C.1 when B.1 finishes; the
	// translated live [B,C] cursor 1 scans 1%2=1 and admits C.1 too.
	e = coreTestEngine(t, 1)
	submitA = coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "D", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	tr = finishAdmitted(t, e, submitA.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.1" {
		t.Fatalf("edge 3 first admission %v, want B.1", tr.Commands)
	}
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "D"})
	if len(e.live) != 2 || len(e.roots) != 4 {
		t.Fatalf("edge 3 live %d roots %d after D stopped", len(e.live), len(e.roots))
	}
	if v, ok := e.View("D.1"); !ok || !v.State.Terminal() || v.Attempts != 0 {
		t.Fatalf("edge 3 stopped leaf %+v %v", v, ok)
	}
	tr = finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.1" {
		t.Fatalf("edge 3 admission after D retired %v, want C.1", tr.Commands)
	}
	finishAdmitted(t, e, tr.Commands[0], StateCompleted)
	if len(e.live) != 0 || len(e.roots) != 4 {
		t.Fatalf("edge 3 live %d roots %d at the end", len(e.live), len(e.roots))
	}

	// Edge 4, several roots retiring in one event: with cap 1 only A.1 is
	// admitted (cursor 1); B and C are all-pending. HostClosing settles and
	// retires both B and C in a single Apply while A stays live cancelling.
	// Cursor equivalence is unobservable here - admission is disabled while
	// closing - so this scenario pins the retirement itself: both pending
	// roots leave the live set in one Apply, e.roots keeps all three roots,
	// and finishing A.1 still reaches Final.
	e = coreTestEngine(t, 1)
	submitA = coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "C", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	tr = coreTestApply(t, e, Event{Type: EventHostClosing})
	if len(tr.Commands) != 1 || tr.Commands[0].Type != CommandStopLeaf || tr.Commands[0].Node != "A.1" {
		t.Fatalf("edge 4 closing commands %v", tr.Commands)
	}
	if len(e.live) != 1 || len(e.roots) != 3 {
		t.Fatalf("edge 4 live %d roots %d after the multi-retirement event", len(e.live), len(e.roots))
	}
	if _, ok := e.View("B"); !ok || !e.nodes["B"].view.State.Terminal() {
		t.Fatal("edge 4 retired root B lost or not terminal")
	}
	tr = finishAdmitted(t, e, submitA.Commands[0], StateCompleted)
	if !tr.Final || !e.final {
		t.Fatalf("edge 4 final not reached: %+v", tr)
	}
	if len(e.live) != 0 || len(e.roots) != 3 {
		t.Fatalf("edge 4 live %d roots %d after final", len(e.live), len(e.roots))
	}
}
