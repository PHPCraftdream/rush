package tasktree

import (
	"errors"
	"reflect"
	"testing"
)

var (
	kernelTestAgent    = Actor{Kind: ActorAgent, ID: "agent"}
	kernelTestOperator = Actor{Kind: ActorOperator, ID: "operator"}
)

func kernelTestLimits() Limits {
	return Limits{MaxNodes: 100, MaxDepth: 12, MaxTitleBytes: 64, MaxReasonBytes: 64, MaxTombstones: 100, MaxReceipts: 100}
}
func kernelTestTask(title string) Draft { return Draft{Kind: KindTask, Title: title} }
func kernelTestGroup(title string, children ...Draft) Draft {
	return Draft{Kind: KindGroup, Title: title, Children: children}
}
func kernelTestString(s string) *string { return &s }
func kernelTestTarget(op Operation, id NodeID) Command {
	return Command{Op: op, Target: Selector{ID: id}}
}

func kernelTestEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
}

func kernelTestTree(t *testing.T, limits Limits, drafts ...Draft) *Tree {
	t.Helper()
	tree, err := Restore(EmptySnapshot(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: drafts})
	}
	return tree
}

func kernelTestApply(t *testing.T, tree *Tree, actor Actor, c Command) Delta {
	t.Helper()
	d, err := tree.Apply(actor, c)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[NodeID]bool)
	for _, ids := range [][]NodeID{d.Created, d.Updated, d.Removed} {
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("duplicate/non-disjoint delta ID %s: %#v", id, d)
			}
			seen[id] = true
		}
	}
	completed := make(map[NodeID]bool)
	for _, id := range d.Completed {
		if completed[id] {
			t.Fatalf("duplicate completion %s", id)
		}
		completed[id] = true
		found := false
		for _, updated := range d.Updated {
			if id == updated {
				found = true
			}
		}
		if !found {
			t.Fatalf("completion %s not updated: %#v", id, d)
		}
	}
	if err := ValidateSnapshot(tree.Snapshot(), tree.limits); err != nil {
		t.Fatalf("accepted command produced invalid snapshot: %v", err)
	}
	return d
}

func kernelTestReject(t *testing.T, tree *Tree, actor Actor, c Command, code ProblemCode) {
	t.Helper()
	before := tree.Snapshot()
	d, err := tree.Apply(actor, c)
	var p *Problem
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("error %v; want %s", err, code)
	}
	kernelTestEqual(t, d, Delta{})
	kernelTestEqual(t, tree.Snapshot(), before)
}

func kernelTestStatus(t *testing.T, tree *Tree, id NodeID, status Status, reason string) {
	t.Helper()
	n := tree.Snapshot().Nodes[id]
	kernelTestEqual(t, n.Status, status)
	kernelTestEqual(t, n.Reason, reason)
}

func TestKernelGuardedLiveDuplicateEdits(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestTask("same"), kernelTestTask("same"), kernelTestTask("other"))
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestOperator, kernelTestTarget(OpRemove, "n2")), Delta{Updated: []NodeID{RootID}, Removed: []NodeID{"n2"}})
	guard := []TitleGuard{{Kind: KindTask, Title: "same"}}
	kernelTestEqual(t, tree.Snapshot().TitleGuards, guard)
	before := tree.Snapshot()
	c := Command{Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("same")}
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, c), Delta{})
	kernelTestEqual(t, tree.Snapshot(), before)
	c.ActiveForm = kernelTestString("working on same")
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, c), Delta{Updated: []NodeID{"n1"}})
	want := CloneSnapshot(before)
	n := want.Nodes["n1"]
	n.ActiveForm = "working on same"
	want.Nodes["n1"] = n
	kernelTestEqual(t, tree.Snapshot(), want)
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestEqual(t, tree.Snapshot().TitleGuards, guard)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n3"}, Title: kernelTestString("same"), ActiveForm: kernelTestString("must not leak")}, CodeRemovedByOperator)
	c.ActiveForm = nil
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestOperator, c), Delta{})
	want.TitleGuards = []TitleGuard{}
	kernelTestEqual(t, tree.Snapshot(), want)
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestStatus(t, tree, "n1", InProgress, "")
	kernelTestStatus(t, tree, "n3", Pending, "")
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n3"}, Title: kernelTestString("same")}), Delta{Updated: []NodeID{"n3"}})
	kernelTestEqual(t, tree.Snapshot().NextID, uint64(4))
	kernelTestReject(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n2"), CodeRemoved)
}

func TestKernelInitialDeltaAndEmptyBoard(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits())
	kernelTestEqual(t, tree.Summary(), Summary{})
	v, err := tree.View(Selector{})
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, v.Root, NodeView{ID: RootID, Kind: KindGroup, Title: "Tasks"})
	d := kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestGroup("empty"), kernelTestGroup("G", kernelTestTask("A"), kernelTestTask("B"))}})
	kernelTestEqual(t, d, Delta{Created: []NodeID{"n1", "n2", "n3", "n4"}, Updated: []NodeID{RootID}})
	kernelTestStatus(t, tree, "n3", InProgress, "")
	kernelTestStatus(t, tree, "n4", Pending, "")
	kernelTestEqual(t, tree.Summary().Progress, Progress{Total: 2, Pending: 1, InProgress: 1, Actionable: 2, Unfinished: 2})
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: RootID}, Reason: "wait"})
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: RootID}, Reason: "wait"}), Delta{})
	kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpUnblock, RootID))
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestGroup("only structure")}})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
	kernelTestEqual(t, tree.Summary().NextID, NodeID("n3"))
}

func TestKernelNestedSelectorsAndStableMoves(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("Phase", kernelTestTask("same"), kernelTestGroup("Inner", kernelTestTask("same"))), kernelTestGroup("Other", kernelTestTask("same")))
	v, err := tree.View(Selector{})
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, v.Root.Children[0].ID, NodeID("n1"))
	kernelTestEqual(t, v.Root.Children[0].Children[1].Children[0].ID, NodeID("n4"))
	kernelTestEqual(t, v.Root.Children[1].Children[0].ID, NodeID("n6"))
	before := tree.Snapshot()
	_, err = tree.View(Selector{Text: "same"})
	var p *Problem
	if !errors.As(err, &p) || p.Code != CodeAmbiguousTarget {
		t.Fatalf("ambiguity: %v", err)
	}
	kernelTestEqual(t, p.Candidates, []Candidate{{ID: "n2", Path: []string{"Tasks", "Phase", "same"}, Status: InProgress}, {ID: "n4", Path: []string{"Tasks", "Phase", "Inner", "same"}, Status: Pending}, {ID: "n6", Path: []string{"Tasks", "Other", "same"}, Status: Pending}})
	kernelTestEqual(t, tree.Snapshot(), before)
	for _, selector := range []Selector{{Text: "Inner", WithinID: "n3"}, {Text: "same", WithinID: "n3"}} {
		v, err = tree.View(selector)
		if err != nil {
			t.Fatal(err)
		}
		want := NodeID("n3")
		if selector.Text == "same" {
			want = "n4"
		}
		kernelTestEqual(t, v.Root.ID, want)
	}
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpDone, Target: Selector{Text: "Same"}}, CodeNotFound)
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n2"}, Title: kernelTestString("renamed")})
	d := kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpMove, Target: Selector{ID: "n2"}, ParentID: "n5", BeforeID: "n6"})
	kernelTestEqual(t, d, Delta{Updated: []NodeID{"n1", "n5", "n2"}})
	kernelTestEqual(t, tree.Snapshot().Nodes["n5"].Children, []NodeID{"n2", "n6"})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n2"))
	kernelTestEqual(t, tree.Summary().ActivePath, []string{"Tasks", "Other", "renamed"})
	d = kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpMove, Target: Selector{ID: "n6"}, ParentID: "n5", BeforeID: "n2"})
	kernelTestEqual(t, d, Delta{Updated: []NodeID{"n5"}})
	kernelTestEqual(t, tree.Snapshot().Nodes["n5"].Children, []NodeID{"n6", "n2"})
}

func TestKernelFocusRewindAndNoNormalization(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestTask("A"), kernelTestTask("B"), kernelTestTask("C"))
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpStart, "n3")), Delta{Updated: []NodeID{"n1", "n3"}})
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n3")), Delta{Updated: []NodeID{"n1", "n3"}, Completed: []NodeID{"n3"}})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: RootID}, Reason: "wait"})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
	kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpUnblock, RootID))
	kernelTestEqual(t, tree.Summary().NextID, NodeID("n1"))
	before := tree.Snapshot()
	_, err := tree.View(Selector{})
	if err != nil {
		t.Fatal(err)
	}
	tree.Summary()
	tree.Briefs([]NodeID{"n1"})
	kernelTestEqual(t, tree.Snapshot(), before)
	for _, c := range []Command{kernelTestTarget(OpUnblock, "n1"), kernelTestTarget(OpDone, "n3"), {Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("A"), ActiveForm: kernelTestString("")}, {Op: OpMove, Target: Selector{ID: "n3"}, ParentID: RootID}} {
		kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, c), Delta{})
		kernelTestEqual(t, tree.Snapshot(), before)
	}
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("AA")})
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpMove, Target: Selector{ID: "n2"}, ParentID: RootID, BeforeID: "n1"})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
	kernelTestEqual(t, tree.Summary().NextID, NodeID("n2"))
	kernelTestApply(t, tree, kernelTestOperator, kernelTestTarget(OpReopen, "n3"))
	kernelTestStatus(t, tree, "n3", Pending, "")
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("D")}})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n2"))
}

func TestKernelBulkAndProgressVectors(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("G", kernelTestTask("A"), kernelTestTask("B"), kernelTestTask("C"), kernelTestTask("D")), kernelTestTask("outside"), kernelTestGroup("empty"))
	kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n3"))
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpDrop, Target: Selector{ID: "n4"}, Reason: "cancel"})
	d := kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n1"}, Reason: "wait"})
	kernelTestEqual(t, d, Delta{Updated: []NodeID{"n2", "n5", "n6"}})
	kernelTestStatus(t, tree, "n3", Completed, "")
	kernelTestStatus(t, tree, "n4", Abandoned, "cancel")
	kernelTestEqual(t, tree.Summary().Progress, Progress{Total: 5, InProgress: 1, Completed: 1, Blocked: 2, Abandoned: 1, Actionable: 1, Unfinished: 3, Settled: 2})
	v, err := tree.View(Selector{ID: "n7"})
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, v.Root.Progress, Progress{})
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpDrop, Target: Selector{ID: "n7"}, Reason: "empty"}), Delta{})
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpDrop, Target: Selector{ID: "n1"}, Reason: "stop"})
	v, err = tree.View(Selector{ID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, v.Root.Progress, Progress{Total: 4, Completed: 1, Abandoned: 3, Settled: 4, AllSettled: true})
	kernelTestStatus(t, tree, "n4", Abandoned, "cancel")
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, ParentID: "n1", Drafts: []Draft{kernelTestTask("new")}})
	v, err = tree.View(Selector{ID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, v.Root.Progress, Progress{Total: 5, Pending: 1, Completed: 1, Abandoned: 3, Actionable: 1, Unfinished: 1, Settled: 4})
	for _, tc := range []struct {
		op     Operation
		reason string
		want   Progress
	}{
		{OpBlock, "wait", Progress{Total: 1, Blocked: 1, Unfinished: 1}},
		{OpDrop, "stop", Progress{Total: 1, Abandoned: 1, Settled: 1, AllSettled: true}},
		{OpDone, "", Progress{Total: 1, Completed: 1, Settled: 1, AllSettled: true, AllCompleted: true}},
	} {
		one := kernelTestTree(t, kernelTestLimits(), kernelTestTask("one"))
		c := kernelTestTarget(tc.op, "n1")
		c.Reason = tc.reason
		kernelTestApply(t, one, kernelTestAgent, c)
		kernelTestEqual(t, one.Summary().Progress, tc.want)
	}
}

func TestKernelRemovalGuardsAndAtomicBatches(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("G", kernelTestTask("same"), kernelTestTask("B")), kernelTestTask("same"))
	for _, tc := range []struct {
		ids  []NodeID
		code ProblemCode
	}{{[]NodeID{"n1", "n1"}, CodeInvalidInput}, {[]NodeID{"n1", "n9"}, CodeNotFound}, {[]NodeID{"n1", RootID}, CodeInvalidTargetKind}} {
		kernelTestReject(t, tree, kernelTestOperator, Command{Op: OpRemove, RemoveIDs: tc.ids}, tc.code)
	}
	d := kernelTestApply(t, tree, kernelTestOperator, Command{Op: OpRemove, RemoveIDs: []NodeID{"n3", "n1", "n2"}})
	kernelTestEqual(t, d, Delta{Updated: []NodeID{RootID, "n4"}, Removed: []NodeID{"n1", "n2", "n3"}})
	kernelTestEqual(t, tree.Snapshot().TitleGuards, []TitleGuard{{Kind: KindGroup, Title: "G"}, {Kind: KindTask, Title: "same"}, {Kind: KindTask, Title: "B"}})
	kernelTestReject(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n2"), CodeRemoved)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("same")}}, CodeRemovedByOperator)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n4"}, Title: kernelTestString("B")}, CodeRemovedByOperator)
	kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n4"))
	d = kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestGroup("same"), kernelTestTask("Same")}})
	kernelTestEqual(t, d.Created, []NodeID{"n5", "n6"})
	kernelTestApply(t, tree, kernelTestOperator, Command{Op: OpEdit, Target: Selector{ID: "n6"}, Title: kernelTestString("B")})
	kernelTestApply(t, tree, kernelTestOperator, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("same")}})
	kernelTestEqual(t, tree.Snapshot().TitleGuards, []TitleGuard{{Kind: KindGroup, Title: "G"}})
	d = kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("same"), kernelTestTask("B")}})
	kernelTestEqual(t, d.Created, []NodeID{"n8", "n9"})
	kernelTestEqual(t, tree.Briefs([]NodeID{"n2", "n8"}), []NodeBrief{{ID: "n2", Removed: true}, {ID: "n8", ParentID: RootID, Kind: KindTask, Title: "same", Status: Pending}})
}
