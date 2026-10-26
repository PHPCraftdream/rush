package jobbatch

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func TestCoreReSeededTreeSettlement(t *testing.T) { coreTestGenerate(t, false, NotifyEach) }

func TestCoreWeSeededAccountingClosedAndOpen(t *testing.T) {
	coreTestGenerate(t, true, NotifyEach)
	t.Run("all", func(t *testing.T) { coreTestGenerate(t, true, NotifyAll) })
}

func coreTestGenerate(t *testing.T, accounting bool, notify Notify) {
	t.Helper()
	for seed := int64(1); seed <= 32; seed++ {
		for _, global := range []int{2, 3} {
			t.Run(fmt.Sprintf("seed-%d/global-%d", seed, global), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				e, err := NewEngine(Limits{MaxConcurrent: global, ByGroup: map[string]int{"a": 1, "b": 1, "": 1}, MaxDepth: 6, MaxNodes: 80})
				if err != nil {
					t.Fatal(err)
				}
				var history []Event
				var transitions []Transition
				defer func() {
					if t.Failed() {
						t.Logf("seed=%d events=%+v", seed, history)
					}
				}()
				issued := map[uint64]bool{}
				noticed := map[ID]bool{}
				apply := func(ev Event) Transition {
					history = append(history, ev)
					before := e.active
					rootStates := map[ID]State{}
					for _, root := range e.roots {
						rootStates[root.view.ID] = root.view.State
					}
					gen := map[string]uint64{}
					for g, v := range e.generation {
						gen[g] = v
					}
					group := ""
					if n := e.nodes[ev.Node]; n != nil && n.task != nil {
						group = n.task.Group
					}
					tr := coreTestApply(t, e, ev)
					transitions = append(transitions, tr)
					delta := 0
					if ev.Type == EventSettled || ev.Type == EventStartRejected {
						delta--
					}
					for _, c := range tr.Commands {
						if c.Type == CommandStartLeaf {
							delta++
							if issued[c.Token] {
								t.Fatal("token reused")
							}
							issued[c.Token] = true
						}
					}
					if e.active != before+delta {
						t.Fatal("unit event accounting")
					}
					if ev.Type == EventSettled {
						gen[group]++
					}
					if ev.Type == EventCapacityAvailable {
						gen[ev.Group]++
					}
					for _, g := range []string{"a", "b", ""} {
						if gen[g] != e.generation[g] {
							t.Fatal("generation event accounting", ev)
						}
					}
					settled := map[ID]Summary{}
					if notify == NotifyAll {
						for _, summary := range tr.SettledRoots {
							if summary.Sequence != tr.Sequence || rootStates[summary.Root].Terminal() || !e.nodes[summary.Root].view.State.Terminal() {
								t.Fatal("root settlement transition", summary, tr)
							}
							settled[summary.Root] = summary
						}
						if len(tr.Notices) != len(settled) {
							t.Fatal("root notice/settlement count", tr)
						}
						for _, root := range e.roots {
							if !rootStates[root.view.ID].Terminal() && root.view.State.Terminal() {
								if _, ok := settled[root.view.ID]; !ok {
									t.Fatal("missing root settlement", root.view.ID, tr)
								}
							}
						}
					}
					for _, notice := range tr.Notices {
						if notify == NotifyAll {
							summary, ok := settled[notice.Node]
							if !ok || notice.Node != notice.Root || notice.Kind != KindRoot || notice.State != summary.State || notice.Counts != summary.Counts || rootStates[notice.Node].Terminal() {
								t.Fatal("notice not exactly newly settled root", notice, tr)
							}
						}
						if noticed[notice.Node] {
							t.Fatal("repeat notice")
						}
						noticed[notice.Node] = true
					}
					if global == 3 && !e.closing {
						for _, n := range e.nodes {
							if n.task != nil && coreTestClausesWithoutGlobal(e, n) {
								t.Fatal("eligible work left")
							}
						}
					}
					return tr
				}
				rootSpec := func(items ...Spec) Spec {
					s := coreTestBatch(ModeParallel, 0, items...)
					s.Batch.Notify = notify
					return s
				}
				groups := []string{"a", "b", ""}
				var spec func(int) Spec
				spec = func(depth int) Spec {
					if depth >= 3 || rng.Intn(3) != 0 {
						return coreTestTask(groups[rng.Intn(3)])
					}
					mode := ModeParallel
					cap := 0
					if rng.Intn(2) == 0 {
						mode = ModeSequential
						cap = rng.Intn(2)
					}
					s := coreTestBatch(mode, cap)
					if mode == ModeSequential && rng.Intn(2) == 0 {
						s.Batch.OnFail = OnFailContinue
					}
					for i := 0; i < rng.Intn(3); i++ {
						s.Batch.Items = append(s.Batch.Items, spec(depth+1))
					}
					return s
				}
				// Force every reject reason and both control results in every seed.
				for i, reason := range []RejectReason{RejectCapacity, RejectFailure, RejectCancelled} {
					root := ID(fmt.Sprintf("probe%d", i))
					tr := apply(Event{Type: EventSubmit, Root: root, Spec: rootSpec(coreTestTask("a"))})
					c := tr.Commands[0]
					code := ""
					if reason != RejectCapacity {
						code = CodeStartFailed
					}
					if reason == RejectCancelled {
						apply(Event{Type: EventStopRequested, Node: c.Node})
						apply(Event{Type: EventControlReported, Node: c.Node, Token: c.Token, Op: ControlOpStop, Code: CodeStopTimeout})
						apply(Event{Type: EventStopRequested, Node: c.Node})
						apply(Event{Type: EventControlReported, Node: c.Node, Token: c.Token, Op: ControlOpStop})
					}
					apply(Event{Type: EventStartRejected, Node: c.Node, Token: c.Token, Reason: reason, Code: code})
					if reason == RejectCapacity {
						apply(Event{Type: EventStopRequested, Node: c.Node})
						apply(Event{Type: EventCapacityAvailable, Group: "a"})
					}
				}
				// A closed profile admits all three groups; an open one leaves only global blocked.
				for i, g := range groups {
					apply(Event{Type: EventSubmit, Root: ID(fmt.Sprintf("base%d", i)), Spec: rootSpec(coreTestTask(g))})
				}
				if global == 2 {
					n := e.nodes["base2.1"]
					if n.view.State != StatePending || e.groups[""] != 0 || e.active != 2 || !coreTestClausesWithoutGlobal(e, n) {
						t.Fatal("missing independent global block")
					}
				}
				if notify == NotifyAll {
					apply(Event{Type: EventAdd, Parent: "base0", Items: []Spec{coreTestTask("a")}})
					n := e.nodes["base0.1"]
					apply(Event{Type: EventStarted, Node: n.view.ID, Token: n.view.Token})
					apply(Event{Type: EventDeadlineExpired, Node: n.view.ID, Token: n.view.Token})
				}
				for i := 0; i < 4; i++ {
					apply(Event{Type: EventSubmit, Root: ID(fmt.Sprintf("tree%d", i)), Spec: rootSpec(spec(1), spec(1), spec(1))})
				}
				for step := 0; step < 180; step++ {
					var live []*treeNode
					for _, root := range e.roots {
						var walk func(*treeNode)
						walk = func(n *treeNode) {
							if !n.view.State.Terminal() {
								live = append(live, n)
							}
							for _, c := range n.children {
								walk(c)
							}
						}
						walk(root)
					}
					if len(live) == 0 {
						break
					}
					n := live[rng.Intn(len(live))]
					switch rng.Intn(7) {
					case 0:
						apply(Event{Type: EventStopRequested, Node: n.view.ID})
					case 1:
						apply(Event{Type: EventCapacityAvailable, Group: groups[rng.Intn(3)]})
					case 2:
						if n.task == nil && n.view.Cause == CauseNone && e.nodeCount(e.nodes[n.view.Root]) < 70 && n.depth < 5 {
							apply(Event{Type: EventAdd, Parent: n.view.ID, Items: []Spec{spec(n.depth)}})
						}
					default:
						if n.task == nil || !n.view.State.Active() {
							continue
						}
						token := n.view.Token
						if n.view.Control == ControlPending {
							codes := []string{"", CodeStopFailed, CodeStopPanic, CodeStopTimeout}
							apply(Event{Type: EventControlReported, Node: n.view.ID, Token: token, Op: ControlOpStop, Code: codes[rng.Intn(len(codes))]})
						} else if !n.accepted {
							if rng.Intn(2) == 0 {
								apply(Event{Type: EventStarted, Node: n.view.ID, Token: token})
							} else {
								reason := RejectCapacity
								code := ""
								if rng.Intn(2) == 0 {
									reason = RejectFailure
									code = CodeStartFailed
								}
								if n.view.Cause != CauseNone && rng.Intn(2) == 0 {
									reason = RejectCancelled
									code = CodeStartFailed
								}
								apply(Event{Type: EventStartRejected, Node: n.view.ID, Token: token, Reason: reason, Code: code})
							}
						} else if accounting && rng.Intn(3) == 0 {
							apply(Event{Type: EventDeadlineExpired, Node: n.view.ID, Token: token})
						} else {
							states := []State{StateCompleted, StateFailed, StateCancelled, StateTimedOut, StateInterrupted}
							apply(Event{Type: EventSettled, Node: n.view.ID, Token: token, Result: Result{State: states[rng.Intn(len(states))]}})
						}
					}
				}
				apply(Event{Type: EventHostClosing})
				for _, root := range e.roots {
					var drain func(*treeNode)
					drain = func(n *treeNode) {
						if n.task == nil {
							for _, c := range n.children {
								drain(c)
							}
							return
						}
						if n.view.State.Terminal() {
							return
						}
						if n.accepted {
							apply(Event{Type: EventSettled, Node: n.view.ID, Token: n.view.Token, Result: Result{State: StateCompleted}})
						} else {
							apply(Event{Type: EventStartRejected, Node: n.view.ID, Token: n.view.Token, Reason: RejectCancelled, Code: CodeStartFailed})
						}
					}
					drain(root)
				}
				if !e.final || e.active != 0 {
					t.Fatal("incomplete drain")
				}
				for _, root := range e.roots {
					if !noticed[root.view.ID] {
						t.Fatal("missing root notice")
					}
				}
				// Same transferred event values replay identically, including the final boundary.
				replay, err := NewEngine(e.limits)
				if err != nil {
					t.Fatal(err)
				}
				for i, ev := range history {
					tr, err := replay.Apply(ev)
					if err != nil {
						t.Fatal("replay", err)
					}
					coreTestInvariant(t, replay, tr)
					if !reflect.DeepEqual(tr, transitions[i]) {
						t.Fatalf("transition mismatch index=%d event=%+v\ngot=%+v\nwant=%+v", i, ev, tr, transitions[i])
					}
				}
				for _, root := range e.roots {
					a, _ := e.Snapshot(root.view.ID)
					b, _ := replay.Snapshot(root.view.ID)
					if !reflect.DeepEqual(a, b) {
						t.Fatal("nondeterminism")
					}
				}
			})
		}
	}
}
