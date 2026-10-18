package jobbatch

import (
	"fmt"
	"reflect"
	"strings"
)

// Engine is a deterministic, single-owner batch state machine.
type Engine struct {
	limits          Limits
	nodes           map[ID]*treeNode
	roots           []*treeNode
	live            []*treeNode
	known           map[ID]bool
	released        map[ID]bool
	sequence, token uint64
	cursor          int
	closing, final  bool
	active          int
	groups          map[string]int
	generation      map[string]uint64
	blocked         map[string]uint64

	// walkInstrument observes the size of each root-forest walk per Apply.
	// It is nil in production; only in-package tests install it.
	walkInstrument func(source string, roots int)
}

// NewEngine validates and takes a detached copy of the scheduling limits.
func NewEngine(l Limits) (*Engine, error) {
	if l.MaxConcurrent <= 0 || l.MaxDepth <= 0 || l.MaxNodes <= 0 {
		return nil, fmt.Errorf("%w: positive limits required", ErrInvalidSpec)
	}
	groups := make(map[string]int, len(l.ByGroup))
	for g, cap := range l.ByGroup {
		if cap <= 0 {
			return nil, fmt.Errorf("%w: positive group limits required", ErrInvalidSpec)
		}
		groups[g] = cap
	}
	l.ByGroup = groups
	return &Engine{limits: l, nodes: map[ID]*treeNode{}, known: map[ID]bool{}, released: map[ID]bool{}, groups: map[string]int{}, generation: map[string]uint64{}, blocked: map[string]uint64{}}, nil
}

type coreTransition struct {
	Transition
	changed []*treeNode
	seen    map[ID]bool
	created map[ID]bool
	settled map[ID]bool
}

func (t *coreTransition) change(n *treeNode) {
	if !t.seen[n.view.ID] {
		t.seen[n.view.ID] = true
		t.changed = append(t.changed, n)
	}
}

// Apply validates the entire event before changing any engine state.
func (e *Engine) Apply(ev Event) (Transition, error) {
	if e.final {
		return Transition{}, fmt.Errorf("%w", ErrClosed)
	}
	staged, n, err := e.validate(ev)
	if err != nil {
		return Transition{}, err
	}
	e.sequence++
	t := &coreTransition{Transition: Transition{Sequence: e.sequence, Event: ev}, seen: map[ID]bool{}, created: map[ID]bool{}, settled: map[ID]bool{}}
	switch ev.Type {
	case EventSubmit:
		e.roots = append(e.roots, staged[0])
		e.live = append(e.live, staged[0])
		e.known[ev.Root] = true
		e.install(staged[0], t)
	case EventAdd:
		for _, child := range staged {
			n.children = append(n.children, child)
			e.install(child, t)
			if n.view.Cause != CauseNone {
				e.cancel(child, n.view.Cause, false, t)
			}
		}
	case EventStopRequested:
		e.cancel(n, CauseStop, true, t)
	case EventDeadlineExpired:
		e.cancel(n, CauseTimeout, false, t)
	case EventHostClosing:
		e.closing = true
		for _, root := range e.roots {
			e.cancel(root, CauseHostClosing, false, t)
		}
	case EventStarted:
		n.accepted = true
		if n.view.Cause == CauseNone {
			n.view.State = StateRunning
		}
		t.change(n)
	case EventStartRejected:
		e.releaseUnit(n)
		n.view.Control = ControlNone
		n.view.ControlPending = false
		n.view.ControlError = ""
		if ev.Reason != RejectCapacity {
			n.view.Outcome = Outcome{State: StateFailed, Code: ev.Code}
		}
		if n.view.Cause != CauseNone {
			e.terminal(n, causeState(n.view.Cause), t)
		} else if ev.Reason == RejectCapacity {
			n.view.State = StatePending
			if e.generation[n.task.Group] == n.authorized {
				e.blocked[n.task.Group] = n.authorized
			}
			t.change(n)
		} else {
			e.terminal(n, StateFailed, t)
		}
	case EventSettled:
		e.releaseUnit(n)
		e.generation[n.task.Group]++
		n.view.Outcome = Outcome{State: ev.Result.State, Code: ev.Result.Code, Summary: ev.Result.Summary}
		state := ev.Result.State
		if n.view.Cause != CauseNone {
			state = causeState(n.view.Cause)
		}
		e.terminal(n, state, t)
	case EventControlReported:
		n.view.ControlPending = false
		n.view.ControlError = ev.Code
		n.view.Control = ControlAcked
		if ev.Code != "" {
			n.view.Control = ControlFailed
		}
		t.change(n)
	case EventCapacityAvailable:
		e.generation[ev.Group]++
	case EventRelease:
		n.released = true
		e.remove(n)
		e.released[ev.Root] = true
	}
	if e.walkInstrument != nil {
		e.walkInstrument("settle", len(e.live))
	}
	for _, root := range e.live {
		e.settle(root, t)
	}
	e.admit(t)
	e.selectNotices(t)
	for _, changed := range t.changed {
		if t.created[changed.view.ID] || !t.settled[changed.view.ID] {
			t.Changed = append(t.Changed, changed.view)
		}
	}
	var settledViews func(*treeNode)
	settledViews = func(n *treeNode) {
		for _, c := range n.children {
			settledViews(c)
		}
		if t.settled[n.view.ID] && !t.created[n.view.ID] {
			t.Changed = append(t.Changed, n.view)
		}
	}
	if e.walkInstrument != nil {
		e.walkInstrument("settledViews", len(e.live))
	}
	for _, root := range e.live {
		settledViews(root)
	}
	e.retireSettledRoots()
	if e.closing {
		t.Final = true
		for _, root := range e.roots {
			if !root.view.State.Terminal() {
				t.Final = false
				break
			}
		}
		e.final = t.Final
	}
	return t.Transition, nil
}

// retireSettledRoots moves terminal roots out of the live scheduling slice
// at the end of an Apply, after their settlement transition, notices and
// child-before-parent changed views were constructed. e.roots keeps every
// retained root exactly as before; only the scheduling walks use live. The
// in-place filter preserves order and translates the round-robin cursor so
// the next scan starts at the same surviving root as before retirement.
func (e *Engine) retireSettledRoots() {
	if len(e.live) == 0 {
		return
	}
	live := e.live[:0]
	kept := 0
	for i, root := range e.live {
		if root.view.State.Terminal() {
			continue
		}
		live = append(live, root)
		if i < e.cursor {
			kept++
		}
	}
	e.live = live
	e.cursor = kept
}

func (e *Engine) validate(ev Event) ([]*treeNode, *treeNode, error) {
	invalid := func() ([]*treeNode, *treeNode, error) {
		return nil, nil, fmt.Errorf("%w: %s", ErrInvalidEvent, ev.Type)
	}
	rest := ev
	rest.Type = ""
	switch ev.Type {
	case EventSubmit:
		rest.Root = ""
		rest.Spec = Spec{}
		rest.Ref = ""
	case EventAdd:
		rest.Parent = ""
		rest.Items = nil
		rest.Ref = ""
	case EventStopRequested:
		rest.Node = ""
	case EventStarted, EventDeadlineExpired:
		rest.Node = ""
		rest.Token = 0
	case EventStartRejected:
		rest.Node = ""
		rest.Token = 0
		rest.Reason = ""
		rest.Code = ""
	case EventSettled:
		rest.Node = ""
		rest.Token = 0
		rest.Result = Result{}
	case EventControlReported:
		rest.Node = ""
		rest.Token = 0
		rest.Op = ""
		rest.Code = ""
	case EventCapacityAvailable:
		rest.Group = ""
	case EventHostClosing:
	case EventRelease:
		rest.Root = ""
	default:
		return invalid()
	}
	if !reflect.DeepEqual(rest, Event{}) || !ValidRef(ev.Ref) {
		return invalid()
	}
	if ev.Type == EventHostClosing {
		if e.closing {
			return invalid()
		}
		return nil, nil, nil
	}
	if ev.Type == EventCapacityAvailable {
		return nil, nil, nil
	}
	if ev.Type == EventSubmit {
		if e.closing {
			return nil, nil, fmt.Errorf("%w", ErrClosing)
		}
		if ev.Root == "" || strings.Contains(string(ev.Root), ".") {
			return nil, nil, fmt.Errorf("%w: root ID", ErrInvalidSpec)
		}
		if e.known[ev.Root] {
			return nil, nil, fmt.Errorf("%w", ErrDuplicateRoot)
		}
		if ev.Spec.Batch == nil || ev.Spec.Task != nil {
			return nil, nil, fmt.Errorf("%w: root must be batch", ErrInvalidSpec)
		}
		count := 0
		root, err := e.build(ev.Spec, ev.Root, nil, 0, 1, &count)
		if err != nil {
			return nil, nil, err
		}
		return []*treeNode{root}, nil, nil
	}
	id := ev.Node
	if ev.Type == EventAdd {
		id = ev.Parent
	}
	if ev.Type == EventRelease {
		id = ev.Root
	}
	n := e.nodes[id]
	if n == nil {
		root, _, _ := strings.Cut(string(id), ".")
		if e.released[ID(root)] {
			return nil, nil, fmt.Errorf("%w", ErrReleased)
		}
		return nil, nil, fmt.Errorf("%w", ErrUnknownNode)
	}
	if ev.Type == EventAdd {
		if e.closing {
			return nil, nil, fmt.Errorf("%w", ErrClosing)
		}
		if n.task != nil {
			return nil, nil, fmt.Errorf("%w", ErrUnsupported)
		}
		if n.view.State.Terminal() {
			return nil, nil, fmt.Errorf("%w", ErrTerminal)
		}
		count := e.nodeCount(e.nodes[n.view.Root])
		staged := make([]*treeNode, 0, len(ev.Items))
		for i, spec := range ev.Items {
			ordinal := len(n.children) + i + 1
			child, err := e.build(spec, ID(fmt.Sprintf("%s.%d", id, ordinal)), n, ordinal, n.depth+1, &count)
			if err != nil {
				return nil, nil, err
			}
			staged = append(staged, child)
		}
		return staged, n, nil
	}
	if ev.Type == EventRelease {
		if n.parent != nil {
			return nil, nil, fmt.Errorf("%w", ErrUnsupported)
		}
		if !n.view.State.Terminal() {
			return nil, nil, fmt.Errorf("%w", ErrNotTerminal)
		}
		return nil, n, nil
	}
	if n.view.State.Terminal() {
		return nil, nil, fmt.Errorf("%w", ErrTerminal)
	}
	if ev.Type == EventStopRequested {
		return nil, n, nil
	}
	if n.task == nil || ev.Token == 0 || ev.Token != n.view.Token || !n.view.State.Active() {
		return invalid()
	}
	switch ev.Type {
	case EventStarted:
		if n.accepted {
			return invalid()
		}
	case EventStartRejected:
		if n.accepted {
			return invalid()
		}
		switch ev.Reason {
		case RejectCapacity:
			if ev.Code != "" {
				return invalid()
			}
		case RejectFailure, RejectCancelled:
			if ev.Code == "" || !ValidCode(ev.Code) || (ReservedCode(ev.Code) && ev.Code != CodeStartFailed && ev.Code != CodeStartNilExecution && ev.Code != CodeStartPanic && ev.Code != CodeInvalid) {
				return invalid()
			}
			if ev.Reason == RejectCancelled && n.view.Cause == CauseNone {
				return invalid()
			}
		default:
			return invalid()
		}
	case EventSettled:
		if !n.accepted || !ev.Result.State.Reportable() || !ValidCode(ev.Result.Code) || (ReservedCode(ev.Result.Code) && ev.Result.Code != CodeInvalid && ev.Result.Code != CodeExecutorInvalidState) {
			return invalid()
		}
	case EventControlReported:
		if n.view.Control != ControlPending || ev.Op != ControlOpStop || (ev.Code != "" && ev.Code != CodeStopFailed && ev.Code != CodeStopPanic && ev.Code != CodeStopTimeout) {
			return invalid()
		}
	case EventDeadlineExpired:
	default:
		return invalid()
	}
	return nil, n, nil
}
