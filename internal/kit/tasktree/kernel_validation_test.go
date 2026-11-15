package tasktree

import (
	"strings"
	"testing"
)

func TestKernelInvalidCommandsActorsRootsAndTransitions(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("G", kernelTestTask("A")), kernelTestTask("B"))
	for _, actor := range []Actor{{}, {Kind: ActorAgent, ID: " \t"}, {Kind: "unknown", ID: "x"}} {
		kernelTestReject(t, tree, actor, kernelTestTarget(OpDone, "n2"), CodeInvalidInput)
	}
	for _, c := range []Command{
		{Op: "unknown"},
		{Op: OpInit},
		{Op: OpAdd},
		{Op: OpDone},
		{Op: OpDone, Target: Selector{ID: "n2", Text: "A"}},
		{Op: OpDone, Target: Selector{ID: "n2", WithinID: "n1"}},
		{Op: OpDone, Target: Selector{WithinID: "n1"}},
		{Op: OpView, Drafts: []Draft{}},
		{Op: OpView, Reason: "x"},
		{Op: OpStart, Target: Selector{ID: "n2"}, Title: kernelTestString("x")},
		{Op: OpDone, Target: Selector{ID: "n2"}, ActiveForm: kernelTestString("x")},
		{Op: OpUnblock, Target: Selector{ID: "n2"}, Reason: "x"},
		{Op: OpReopen, Target: Selector{ID: "n2"}, ParentID: RootID},
		{Op: OpBlock, Target: Selector{ID: "n2"}, Reason: " \t"},
		{Op: OpDrop, Target: Selector{ID: "n2"}, Reason: "x", BeforeID: "n3"},
		{Op: OpEdit, Target: Selector{ID: "n2"}},
		{Op: OpEdit, Target: Selector{ID: "n2"}, RemoveIDs: []NodeID{}},
		{Op: OpMove, Target: Selector{ID: "n2"}},
		{Op: OpAdd, Target: Selector{ID: RootID}, Drafts: []Draft{kernelTestTask("x")}},
		{Op: OpInit, ParentID: RootID, Drafts: []Draft{kernelTestTask("x")}},
		{Op: OpRemove, Target: Selector{ID: "n2"}, RemoveIDs: []NodeID{"n3"}},
		{Op: OpRemove, RemoveIDs: []NodeID{}},
	} {
		kernelTestReject(t, tree, kernelTestOperator, c, CodeInvalidInput)
	}
	for _, op := range []Operation{OpRemove, OpReopen} {
		kernelTestReject(t, tree, kernelTestAgent, kernelTestTarget(op, "n2"), CodeForbidden)
	}
	for _, op := range []Operation{OpStart, OpDone, OpEdit, OpMove, OpRemove, OpReopen} {
		c := kernelTestTarget(op, RootID)
		if op == OpEdit {
			c.Title = kernelTestString("Tasks")
		}
		if op == OpMove {
			c.ParentID = "n1"
		}
		kernelTestReject(t, tree, kernelTestOperator, c, CodeInvalidTargetKind)
	}
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n1"}, ActiveForm: kernelTestString("x")}, CodeInvalidTargetKind)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, ParentID: "n2", Drafts: []Draft{kernelTestTask("x")}}, CodeInvalidTargetKind)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpDone, Target: Selector{Text: "A", WithinID: "n2"}}, CodeInvalidTargetKind)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("x")}}, CodeAlreadyInitialized)
	empty := kernelTestTree(t, kernelTestLimits())
	kernelTestReject(t, empty, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("x")}}, CodeUninitialized)
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n2"}, Reason: "wait"})
	kernelTestReject(t, tree, kernelTestAgent, kernelTestTarget(OpStart, "n2"), CodeInvalidTransition)
	kernelTestApply(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n2"))
	for _, op := range []Operation{OpBlock, OpDrop, OpStart} {
		c := kernelTestTarget(op, "n2")
		if op != OpStart {
			c.Reason = "x"
		}
		kernelTestReject(t, tree, kernelTestAgent, c, CodeInvalidTransition)
	}
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpDrop, Target: Selector{ID: "n3"}, Reason: "stop"})
	kernelTestReject(t, tree, kernelTestAgent, kernelTestTarget(OpDone, "n3"), CodeInvalidTransition)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n3"}, Reason: "x"}, CodeInvalidTransition)
	kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpDrop, Target: Selector{ID: "n3"}, Reason: "other"}), Delta{})
	kernelTestApply(t, tree, kernelTestOperator, kernelTestTarget(OpReopen, "n3"))
	kernelTestReject(t, tree, kernelTestOperator, kernelTestTarget(OpReopen, "n3"), CodeInvalidTransition)
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
}

func TestKernelMoveValidationAndDepthBoundaries(t *testing.T) {
	limits := kernelTestLimits()
	limits.MaxDepth = 3
	tree := kernelTestTree(t, limits, kernelTestGroup("A", kernelTestGroup("B", kernelTestTask("leaf"))), kernelTestGroup("C"), kernelTestTask("tail"))
	for _, tc := range []struct {
		id, parent, before NodeID
		code               ProblemCode
	}{
		{"n1", "n1", "", CodeInvalidInput},
		{"n1", "n2", "", CodeInvalidInput},
		{"n3", "n2", "n3", CodeInvalidInput},
		{"n3", "n4", "n2", CodeInvalidInput},
		{"n3", "n4", "n9", CodeNotFound},
		{"n3", "n9", "", CodeNotFound},
		{"n3", "n5", "", CodeInvalidTargetKind},
		{"n4", "n2", "", CodeLimitExceeded},
	} {
		if tc.id == "n4" {
			kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, ParentID: "n4", Drafts: []Draft{kernelTestTask("child")}})
		}
		kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpMove, Target: Selector{ID: tc.id}, ParentID: tc.parent, BeforeID: tc.before}, tc.code)
	}
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpMove, Target: Selector{ID: "n4"}, ParentID: "n1"})
	kernelTestEqual(t, tree.Snapshot().Nodes["n4"].ParentID, NodeID("n1"))
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n3"))
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, ParentID: "n2", Drafts: []Draft{kernelTestGroup("deep", kernelTestTask("too deep"))}}, CodeLimitExceeded)
}

func TestKernelExactLimitsAndLateDraftAtomicity(t *testing.T) {
	limits := kernelTestLimits()
	limits.MaxNodes = 3
	limits.MaxDepth = 2
	limits.MaxTitleBytes = 2
	limits.MaxReasonBytes = 2
	tree := kernelTestTree(t, limits, kernelTestGroup("GG", kernelTestTask("é")))
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("x")}}, CodeLimitExceeded)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n2"}, Title: kernelTestString("éx")}, CodeLimitExceeded)
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n2"}, Title: kernelTestString(" \t")}, CodeInvalidInput)
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n2"}, Reason: "é"})
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n2"}, Reason: "éx"}, CodeLimitExceeded)
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n2"}, ActiveForm: kernelTestString("é")})
	kernelTestEqual(t, tree.Snapshot().Nodes["n2"].ActiveForm, "é")
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n2"}, ActiveForm: kernelTestString("éx")}, CodeLimitExceeded)
	fresh := kernelTestTree(t, limits, Draft{Kind: KindTask, Title: "ok", ActiveForm: "é"})
	kernelTestEqual(t, fresh.Snapshot().Nodes["n1"].ActiveForm, "é")
	if _, err := Restore(fresh.Snapshot(), limits); err != nil {
		t.Fatal(err)
	}
	wide := kernelTestTree(t, kernelTestLimits(), kernelTestTask("guard"))
	kernelTestApply(t, wide, kernelTestOperator, kernelTestTarget(OpRemove, "n1"))
	for _, bad := range []Draft{{Kind: "bad", Title: "x"}, {Kind: KindTask, Title: " \t"}, {Kind: KindGroup, Title: "x", ActiveForm: "bad"}, {Kind: KindTask, Title: "x", Children: []Draft{kernelTestTask("bad")}}} {
		before := wide.Snapshot()
		kernelTestReject(t, wide, kernelTestOperator, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("guard"), kernelTestGroup("nested", kernelTestGroup("deep", bad))}}, CodeInvalidInput)
		kernelTestEqual(t, wide.Snapshot(), before)
		kernelTestReject(t, wide, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("guard")}}, CodeRemovedByOperator)
	}
	d := kernelTestApply(t, wide, kernelTestOperator, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("guard")}})
	kernelTestEqual(t, d.Created, []NodeID{"n2"})
	uninitialized := kernelTestTree(t, kernelTestLimits())
	kernelTestReject(t, uninitialized, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("ok"), kernelTestGroup("G", Draft{Kind: KindTask, Title: ""})}}, CodeInvalidInput)
	kernelTestEqual(t, uninitialized.Snapshot(), EmptySnapshot())
}

func TestKernelActiveFormLimitsAndAtomicity(t *testing.T) {
	limits := kernelTestLimits()
	limits.MaxTitleBytes = 8
	for _, form := range []string{"", " \t", "éééé"} {
		t.Run("accepted/"+form, func(t *testing.T) {
			tree := kernelTestTree(t, limits, Draft{Kind: KindTask, Title: "A", ActiveForm: form})
			kernelTestEqual(t, tree.Snapshot().Nodes["n1"].ActiveForm, form)
			c := Command{Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("A"), ActiveForm: &form}
			before := tree.Snapshot()
			kernelTestEqual(t, kernelTestApply(t, tree, kernelTestAgent, c), Delta{})
			kernelTestEqual(t, tree.Snapshot(), before)
			c.ActiveForm = kernelTestString("")
			kernelTestApply(t, tree, kernelTestAgent, c)
			kernelTestEqual(t, tree.Snapshot().Nodes["n1"].ActiveForm, "")
			kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
			kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{{Kind: KindTask, Title: "B", ActiveForm: form}}})
			kernelTestEqual(t, tree.Snapshot().Nodes["n2"].ActiveForm, form)
		})
	}
	for _, form := range []string{"ééééx", strings.Repeat("x", 1<<20)} {
		t.Run("rejected", func(t *testing.T) {
			seed := EmptySnapshot()
			seed.TitleGuards = []TitleGuard{{Kind: KindTask, Title: "guard"}}
			tree, err := Restore(seed, limits)
			if err != nil {
				t.Fatal(err)
			}
			drafts := []Draft{kernelTestTask("guard"), kernelTestGroup("G", Draft{Kind: KindTask, Title: "bad", ActiveForm: form})}
			kernelTestReject(t, tree, kernelTestOperator, Command{Op: OpInit, Drafts: drafts}, CodeLimitExceeded)
			kernelTestEqual(t, tree.Summary().ActiveID, NodeID(""))
			kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("guard")}}, CodeRemovedByOperator)
			kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("A")}})
			kernelTestReject(t, tree, kernelTestOperator, Command{Op: OpAdd, Drafts: drafts}, CodeLimitExceeded)
			kernelTestReject(t, tree, kernelTestOperator, Command{Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("guard"), ActiveForm: &form}, CodeLimitExceeded)
			kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
			kernelTestEqual(t, tree.Snapshot().NextID, uint64(2))
			kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("guard")}}, CodeRemovedByOperator)
		})
	}
	for _, form := range []string{"", " \t", strings.Repeat("x", 9)} {
		tree := kernelTestTree(t, limits, kernelTestGroup("G"))
		kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpEdit, Target: Selector{ID: "n1"}, ActiveForm: &form}, CodeInvalidTargetKind)
		if form != "" {
			kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{{Kind: KindGroup, Title: "G", ActiveForm: form}}}, CodeInvalidInput)
		}
	}
}

func TestKernelIndependentHistoryLimits(t *testing.T) {
	limits := kernelTestLimits()
	limits.MaxTombstones = 2
	tree := kernelTestTree(t, limits, kernelTestTask("same"), kernelTestTask("same"), kernelTestTask("third"))
	kernelTestApply(t, tree, kernelTestOperator, Command{Op: OpRemove, RemoveIDs: []NodeID{"n1", "n2"}})
	kernelTestEqual(t, len(tree.Snapshot().TitleGuards), 1)
	kernelTestReject(t, tree, kernelTestOperator, kernelTestTarget(OpRemove, "n3"), CodeLimitExceeded)
	s := EmptySnapshot()
	s.Initialized = true
	s.NextID = 20
	s.Nodes[RootID] = Node{ID: RootID, Kind: KindGroup, Title: "Tasks", Children: []NodeID{"n1"}}
	s.Nodes["n1"] = Node{ID: "n1", ParentID: RootID, Kind: KindTask, Title: "third", Status: Pending}
	s.TitleGuards = []TitleGuard{{Kind: KindTask, Title: "one"}, {Kind: KindTask, Title: "two"}}
	guardFull, err := Restore(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	kernelTestReject(t, guardFull, kernelTestOperator, kernelTestTarget(OpRemove, "n1"), CodeLimitExceeded)
	kernelTestApply(t, guardFull, kernelTestOperator, Command{Op: OpEdit, Target: Selector{ID: "n1"}, Title: kernelTestString("one")})
	kernelTestApply(t, guardFull, kernelTestOperator, kernelTestTarget(OpRemove, "n1"))
	kernelTestEqual(t, len(guardFull.Snapshot().Tombstones), 1)
	kernelTestEqual(t, len(guardFull.Snapshot().TitleGuards), 2)
}

func TestKernelSparseCountersAndOverflow(t *testing.T) {
	s := EmptySnapshot()
	s.Initialized = true
	s.NextID = ^uint64(0)
	s.Nodes[RootID] = Node{ID: RootID, Kind: KindGroup, Title: "Tasks", Children: []NodeID{"n1"}}
	s.Nodes["n1"] = Node{ID: "n1", ParentID: RootID, Kind: KindTask, Title: "A", Status: Pending}
	tree, err := Restore(s, kernelTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("B")}}, CodeLimitExceeded)
	s.NextID--
	tree, err = Restore(s, kernelTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("B"), kernelTestTask("C")}}, CodeLimitExceeded)
	d := kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("B")}})
	kernelTestEqual(t, d.Created, []NodeID{"n3w5e11264sgse"})
	kernelTestEqual(t, tree.Snapshot().NextID, ^uint64(0))
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("C")}}, CodeLimitExceeded)
}
