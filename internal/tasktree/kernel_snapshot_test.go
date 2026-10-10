package tasktree

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestKernelGuardedUninitializedImport(t *testing.T) {
	limits := kernelTestLimits()
	s := EmptySnapshot()
	s.TitleGuards = []TitleGuard{{Kind: KindTask, Title: "guarded"}, {Kind: KindGroup, Title: "unrelated"}}
	before := CloneSnapshot(s)
	tree, err := Restore(s, limits)
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, tree.Snapshot(), before)
	kernelTestEqual(t, tree.Summary(), Summary{})
	kernelTestReject(t, tree, kernelTestAgent, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("guarded")}}, CodeRemovedByOperator)
	kernelTestEqual(t, tree.Snapshot(), before)
	d := kernelTestApply(t, tree, kernelTestOperator, Command{Op: OpInit, Drafts: []Draft{kernelTestTask("guarded")}})
	kernelTestEqual(t, d, Delta{Created: []NodeID{"n1"}, Updated: []NodeID{RootID}})
	kernelTestEqual(t, tree.Snapshot().Initialized, true)
	kernelTestEqual(t, tree.Snapshot().NextID, uint64(2))
	kernelTestEqual(t, tree.Snapshot().Nodes["n1"], Node{ID: "n1", ParentID: RootID, Kind: KindTask, Title: "guarded", Status: InProgress})
	kernelTestEqual(t, tree.Summary().ActiveID, NodeID("n1"))
	kernelTestEqual(t, tree.Snapshot().TitleGuards, []TitleGuard{{Kind: KindGroup, Title: "unrelated"}})
	kernelTestEqual(t, s, before)
	kernelTestEqual(t, EmptySnapshot().TitleGuards, []TitleGuard{})
	for _, tc := range []struct {
		name   string
		guards []TitleGuard
		limit  int
		code   ProblemCode
	}{
		{"kind", []TitleGuard{{Kind: "bad", Title: "guarded"}}, 100, CodeInvalidSnapshot},
		{"blank", []TitleGuard{{Kind: KindTask, Title: " \t"}}, 100, CodeInvalidSnapshot},
		{"long", []TitleGuard{{Kind: KindTask, Title: string(make([]byte, 65))}}, 100, CodeInvalidSnapshot},
		{"duplicate", []TitleGuard{{Kind: KindTask, Title: "guarded"}, {Kind: KindTask, Title: "guarded"}}, 100, CodeInvalidSnapshot},
		{"historyLimit", []TitleGuard{{Kind: KindTask, Title: "one"}, {Kind: KindGroup, Title: "two"}}, 1, CodeLimitExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := EmptySnapshot()
			bad.TitleGuards = tc.guards
			before := CloneSnapshot(bad)
			bounded := limits
			bounded.MaxTombstones = tc.limit
			for _, validate := range []func() error{func() error { return ValidateSnapshot(bad, bounded) }, func() error { _, err := Restore(bad, bounded); return err }} {
				err := validate()
				var p *Problem
				if !errors.As(err, &p) || p.Code != tc.code {
					t.Fatalf("got %v; want %s", err, tc.code)
				}
				kernelTestEqual(t, bad, before)
			}
		})
	}
}

func TestKernelActiveFormSnapshotImport(t *testing.T) {
	limits := kernelTestLimits()
	limits.MaxTitleBytes = 8
	base := kernelTestTree(t, limits, kernelTestTask("A")).Snapshot()
	for _, form := range []string{"", " \t", "éééé", "ééééx", strings.Repeat("x", 1<<20)} {
		s := CloneSnapshot(base)
		n := s.Nodes["n1"]
		n.ActiveForm = form
		s.Nodes["n1"] = n
		before := CloneSnapshot(s)
		for _, validate := range []func() error{func() error { return ValidateSnapshot(s, limits) }, func() error { _, err := Restore(s, limits); return err }} {
			err := validate()
			if len(form) > limits.MaxTitleBytes {
				var p *Problem
				if !errors.As(err, &p) || p.Code != CodeInvalidSnapshot {
					t.Fatalf("oversized import: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			kernelTestEqual(t, s, before)
		}
	}
}

func TestKernelValidateLimitsEveryField(t *testing.T) {
	base := kernelTestLimits()
	if err := ValidateLimits(base); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"MaxNodes", "MaxDepth", "MaxTitleBytes", "MaxReasonBytes", "MaxTombstones", "MaxReceipts"} {
		for _, value := range []int64{0, -1} {
			t.Run(name+map[int64]string{0: "Zero", -1: "Negative"}[value], func(t *testing.T) {
				limits := base
				reflect.ValueOf(&limits).Elem().Field(i).SetInt(value)
				err := ValidateLimits(limits)
				var p *Problem
				if !errors.As(err, &p) || p.Code != CodeInvalidInput {
					t.Fatalf("got %v", err)
				}
				if _, err := Restore(EmptySnapshot(), limits); err == nil {
					t.Fatal("Restore accepted invalid limits")
				}
			})
		}
	}
}

func TestKernelSnapshotCorruptionMatrix(t *testing.T) {
	base := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("G", kernelTestTask("A")), kernelTestTask("B")).Snapshot()
	change := func(s *Snapshot, id NodeID, f func(*Node)) { n := s.Nodes[id]; f(&n); s.Nodes[id] = n }
	cases := []struct {
		name    string
		corrupt func(*Snapshot)
	}{
		{"schema", func(s *Snapshot) { s.SchemaVersion = 2 }},
		{"rootID", func(s *Snapshot) { s.RootID = "n1" }},
		{"missingRoot", func(s *Snapshot) { delete(s.Nodes, RootID) }},
		{"rootTitle", func(s *Snapshot) { change(s, RootID, func(n *Node) { n.Title = "Other" }) }},
		{"rootParent", func(s *Snapshot) { change(s, RootID, func(n *Node) { n.ParentID = "n1" }) }},
		{"rootKind", func(s *Snapshot) { change(s, RootID, func(n *Node) { n.Kind = KindTask }) }},
		{"zeroCounter", func(s *Snapshot) { s.NextID = 0 }},
		{"liveCounterReuse", func(s *Snapshot) { s.NextID = 3 }},
		{"keyMismatch", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.ID = "n3" }) }},
		{"noncanonicalID", func(s *Snapshot) {
			n := s.Nodes["n3"]
			delete(s.Nodes, "n3")
			n.ID = "n03"
			s.Nodes["n03"] = n
			change(s, RootID, func(n *Node) { n.Children[1] = "n03" })
		}},
		{"uppercaseID", func(s *Snapshot) {
			n := s.Nodes["n3"]
			delete(s.Nodes, "n3")
			n.ID = "nA"
			s.Nodes["nA"] = n
			s.NextID = 20
			change(s, RootID, func(n *Node) { n.Children[1] = "nA" })
		}},
		{"overflowID", func(s *Snapshot) {
			n := s.Nodes["n3"]
			delete(s.Nodes, "n3")
			n.ID = "nzzzzzzzzzzzzzzzzzzzz"
			s.Nodes[n.ID] = n
			change(s, RootID, func(root *Node) { root.Children[1] = n.ID })
		}},
		{"overlap", func(s *Snapshot) {
			s.Tombstones["n2"] = Tombstone{ID: "n2", Kind: KindTask, Title: "A", Actor: kernelTestOperator}
		}},
		{"tombKeyMismatch", func(s *Snapshot) {
			s.Tombstones["n9"] = Tombstone{ID: "n8", Kind: KindTask, Title: "gone", Actor: kernelTestOperator}
			s.NextID = 10
		}},
		{"tombCounterReuse", func(s *Snapshot) {
			s.Tombstones["n4"] = Tombstone{ID: "n4", Kind: KindTask, Title: "gone", Actor: kernelTestOperator}
		}},
		{"tombRoot", func(s *Snapshot) {
			s.Tombstones[RootID] = Tombstone{ID: RootID, Kind: KindGroup, Title: "Tasks", Actor: kernelTestOperator}
		}},
		{"tombActor", func(s *Snapshot) {
			s.Tombstones["n4"] = Tombstone{ID: "n4", Kind: KindTask, Title: "gone", Actor: kernelTestAgent}
			s.NextID = 5
		}},
		{"tombBlankActor", func(s *Snapshot) {
			s.Tombstones["n4"] = Tombstone{ID: "n4", Kind: KindTask, Title: "gone", Actor: Actor{Kind: ActorOperator}}
			s.NextID = 5
		}},
		{"tombKind", func(s *Snapshot) {
			s.Tombstones["n4"] = Tombstone{ID: "n4", Kind: "bad", Title: "gone", Actor: kernelTestOperator}
			s.NextID = 5
		}},
		{"tombTitle", func(s *Snapshot) {
			s.Tombstones["n4"] = Tombstone{ID: "n4", Kind: KindTask, Title: " ", Actor: kernelTestOperator}
			s.NextID = 5
		}},
		{"unknownChild", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Children = []NodeID{"n9"} }) }},
		{"repeatedChild", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Children = []NodeID{"n2", "n2"} }) }},
		{"crossParentRepeat", func(s *Snapshot) { change(s, RootID, func(n *Node) { n.Children = append(n.Children, "n2") }) }},
		{"parentMismatch", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.ParentID = RootID }) }},
		{"parentMissing", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.ParentID = "" }) }},
		{"orphan", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Children = nil }) }},
		{"rootCycle", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Children = append(n.Children, RootID) }) }},
		{"detachedCycle", func(s *Snapshot) {
			change(s, RootID, func(n *Node) { n.Children = []NodeID{"n3"} })
			change(s, "n1", func(n *Node) { n.ParentID = "n4" })
			s.Nodes["n4"] = Node{ID: "n4", ParentID: "n1", Kind: KindGroup, Title: "cycle", Children: []NodeID{"n1"}}
			change(s, "n1", func(n *Node) { n.Children = append(n.Children, "n4") })
			s.NextID = 5
		}},
		{"multipleFocus", func(s *Snapshot) { change(s, "n3", func(n *Node) { n.Status = InProgress }) }},
		{"badStatus", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Status = "unknown" }) }},
		{"badKind", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Kind = "unknown" }) }},
		{"taskChildren", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Children = []NodeID{"n3"} }) }},
		{"openReason", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Reason = "unexpected" }) }},
		{"completedReason", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Status = Completed; n.Reason = "unexpected" }) }},
		{"blockedBlankReason", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Status = Blocked; n.Reason = " \t" }) }},
		{"abandonedMissingReason", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Status = Abandoned }) }},
		{"longReason", func(s *Snapshot) {
			change(s, "n2", func(n *Node) { n.Status = Blocked; n.Reason = string(make([]byte, 65)) })
		}},
		{"blankTitle", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Title = " \t" }) }},
		{"longTitle", func(s *Snapshot) { change(s, "n2", func(n *Node) { n.Title = string(make([]byte, 65)) }) }},
		{"groupStatus", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Status = Pending }) }},
		{"groupReason", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.Reason = "bad" }) }},
		{"groupForm", func(s *Snapshot) { change(s, "n1", func(n *Node) { n.ActiveForm = "bad" }) }},
		{"guardKind", func(s *Snapshot) { s.TitleGuards = []TitleGuard{{Kind: "bad", Title: "gone"}} }},
		{"guardBlank", func(s *Snapshot) { s.TitleGuards = []TitleGuard{{Kind: KindTask, Title: " "}} }},
		{"guardDuplicate", func(s *Snapshot) {
			s.TitleGuards = []TitleGuard{{Kind: KindTask, Title: "gone"}, {Kind: KindTask, Title: "gone"}}
		}},
		{"uninitializedLive", func(s *Snapshot) { s.Initialized = false }},
		{"uninitializedCounter", func(s *Snapshot) { *s = EmptySnapshot(); s.NextID = 2 }},
		{"uninitializedTomb", func(s *Snapshot) {
			*s = EmptySnapshot()
			s.NextID = 2
			s.Tombstones["n1"] = Tombstone{ID: "n1", Kind: KindTask, Title: "gone", Actor: kernelTestOperator}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := CloneSnapshot(base)
			tc.corrupt(&s)
			before := CloneSnapshot(s)
			for _, validate := range []func() error{func() error { return ValidateSnapshot(s, kernelTestLimits()) }, func() error { _, err := Restore(s, kernelTestLimits()); return err }} {
				err := validate()
				var p *Problem
				if !errors.As(err, &p) || p.Code != CodeInvalidSnapshot {
					t.Fatalf("got %v; want invalid_snapshot", err)
				}
				kernelTestEqual(t, s, before)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		limits Limits
	}{
		{"nodes", Limits{MaxNodes: 3, MaxDepth: 12, MaxTitleBytes: 64, MaxReasonBytes: 64, MaxTombstones: 100, MaxReceipts: 100}},
		{"depth", Limits{MaxNodes: 100, MaxDepth: 1, MaxTitleBytes: 64, MaxReasonBytes: 64, MaxTombstones: 100, MaxReceipts: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := CloneSnapshot(base)
			err := ValidateSnapshot(base, tc.limits)
			var p *Problem
			if !errors.As(err, &p) || p.Code != CodeLimitExceeded {
				t.Fatalf("got %v", err)
			}
			kernelTestEqual(t, base, before)
		})
	}
}

func kernelTestDamageSnapshot(s *Snapshot) {
	n := s.Nodes[RootID]
	n.Children[0] = "bad"
	s.Nodes[RootID] = n
	delete(s.Nodes, "n2")
	delete(s.Tombstones, "n4")
	s.TitleGuards[0].Title = "bad"
}

func TestKernelDetachedBoundariesAndJSONRoundtrip(t *testing.T) {
	tree := kernelTestTree(t, kernelTestLimits(), kernelTestGroup("G", kernelTestTask("A"), kernelTestTask("B")), kernelTestTask("gone"))
	kernelTestApply(t, tree, kernelTestAgent, Command{Op: OpBlock, Target: Selector{ID: "n2"}, Reason: "waiting on review"})
	kernelTestApply(t, tree, kernelTestOperator, kernelTestTarget(OpRemove, "n4"))
	want := tree.Snapshot()
	input := CloneSnapshot(want)
	restored, err := Restore(input, kernelTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	kernelTestDamageSnapshot(&input)
	kernelTestEqual(t, restored.Snapshot(), want)
	export := restored.Snapshot()
	kernelTestDamageSnapshot(&export)
	kernelTestEqual(t, restored.Snapshot(), want)
	clone := CloneSnapshot(want)
	kernelTestDamageSnapshot(&clone)
	kernelTestEqual(t, tree.Snapshot(), want)
	v, err := restored.View(Selector{})
	if err != nil {
		t.Fatal(err)
	}
	v.Root.Children[0].Title = "bad"
	v.Root.Children[0].Children[0].Reason = "bad"
	v.Summary.ActivePath[0] = "bad"
	v.Summary.NextPath[0] = "bad"
	summary := restored.Summary()
	summary.ActivePath[0] = "bad"
	summary.NextPath[0] = "bad"
	briefs := restored.Briefs([]NodeID{"n2", "n4"})
	briefs[0].Title = "bad"
	briefs[1].Removed = false
	kernelTestEqual(t, restored.Snapshot(), want)
	kernelTestEqual(t, restored.Summary().ActivePath, []string{"Tasks", "G", "B"})
	kernelTestEqual(t, restored.Briefs([]NodeID{"n2", "n4"}), []NodeBrief{{ID: "n2", ParentID: "n1", Kind: KindTask, Title: "A", Status: Blocked, Reason: "waiting on review"}, {ID: "n4", Removed: true}})
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	loaded, err := Restore(decoded, kernelTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	kernelTestEqual(t, loaded.Snapshot().Nodes["n1"].Children, []NodeID{"n2", "n3"})
	kernelTestStatus(t, loaded, "n2", Blocked, "waiting on review")
	kernelTestEqual(t, loaded.Snapshot().TitleGuards, []TitleGuard{{Kind: KindTask, Title: "gone"}})
	kernelTestEqual(t, loaded.Snapshot().Tombstones, want.Tombstones)
	kernelTestEqual(t, loaded.Snapshot().NextID, uint64(5))
	kernelTestReject(t, loaded, kernelTestAgent, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("gone")}}, CodeRemovedByOperator)
	d := kernelTestApply(t, loaded, kernelTestOperator, Command{Op: OpAdd, Drafts: []Draft{kernelTestTask("gone")}})
	kernelTestEqual(t, d.Created, []NodeID{"n5"})
	kernelTestEqual(t, tree.Snapshot(), want)
	kernelTestEqual(t, restored.Snapshot(), want)
}

func TestKernelDraftOwnershipAndSnapshotHistoryBounds(t *testing.T) {
	drafts := []Draft{kernelTestGroup("G", kernelTestTask("A"))}
	tree := kernelTestTree(t, kernelTestLimits(), drafts...)
	want := tree.Snapshot()
	drafts[0].Title = "bad"
	drafts[0].Children[0].Title = "bad"
	drafts[0].Children = append(drafts[0].Children, kernelTestTask("bad"))
	kernelTestEqual(t, tree.Snapshot(), want)
	limits := kernelTestLimits()
	limits.MaxTombstones = 1
	for _, history := range []string{"tombstones", "guards"} {
		t.Run(history, func(t *testing.T) {
			s := EmptySnapshot()
			s.Initialized = true
			s.NextID = 10
			if history == "tombstones" {
				s.Tombstones["n1"] = Tombstone{ID: "n1", Kind: KindTask, Title: "one", Actor: kernelTestOperator}
				s.Tombstones["n2"] = Tombstone{ID: "n2", Kind: KindTask, Title: "two", Actor: kernelTestOperator}
			} else {
				s.TitleGuards = []TitleGuard{{Kind: KindTask, Title: "one"}, {Kind: KindTask, Title: "two"}}
			}
			before := CloneSnapshot(s)
			err := ValidateSnapshot(s, limits)
			var p *Problem
			if !errors.As(err, &p) || p.Code != CodeLimitExceeded {
				t.Fatalf("got %v", err)
			}
			kernelTestEqual(t, s, before)
		})
	}
}
