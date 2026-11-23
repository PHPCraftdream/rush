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

// dropTaskPayloads releases Engine ownership of task payloads. Payload is
// never a view field, so View and Snapshot observables are unchanged.
func (e *Engine) dropTaskPayloads() {
	for _, n := range e.nodes {
		if n.task != nil {
			n.task.Payload = nil
		}
	}
}

// subtreeStopRetryable reports whether any node under id carries a failed
// control awaiting the cancel retry re-arm (tree_apply.go:55), so a
// redundant Stop must still be applied to re-issue it.
func (e *Engine) subtreeStopRetryable(id ID) bool {
	n := e.nodes[id]
	if n == nil {
		return false
	}
	var walk func(*treeNode) bool
	walk = func(p *treeNode) bool {
		if p.view.Control == ControlFailed {
			return true
		}
		for _, c := range p.children {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(n)
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

type treeNode struct {
	view               NodeView
	task               *TaskSpec
	parent             *treeNode
	children           []*treeNode
	depth              int
	authorized         uint64
	accepted, released bool
}

func (e *Engine) build(s Spec, id ID, parent *treeNode, ordinal, depth int, count *int) (*treeNode, error) {
	bad := func() (*treeNode, error) { return nil, fmt.Errorf("%w: subtree", ErrInvalidSpec) }
	*count++
	if depth > e.limits.MaxDepth || *count > e.limits.MaxNodes || (s.Task == nil) == (s.Batch == nil) {
		return bad()
	}
	n := &treeNode{parent: parent, depth: depth, view: NodeView{ID: id, Root: id, Ordinal: ordinal, State: StatePending}}
	if parent != nil {
		n.view.Parent = parent.view.ID
		n.view.Root = parent.view.Root
	}
	if s.Task != nil {
		if s.Task.Kind == "" || s.Task.Timeout < 0 {
			return bad()
		}
		task := *s.Task
		n.task = &task
		n.view.Kind = KindTask
		n.view.Group = task.Group
		n.view.Control = ControlNone
		return n, nil
	}
	b := *s.Batch
	if b.Mode != ModeParallel && b.Mode != ModeSequential {
		return bad()
	}
	if b.Notify == "" {
		b.Notify = NotifyAll
	}
	if b.Notify != NotifyAll && b.Notify != NotifyEach {
		return bad()
	}
	if b.MaxParallel < 0 {
		return bad()
	}
	if b.Mode == ModeParallel {
		if b.OnFail != "" {
			return nil, fmt.Errorf("%w: on_fail is only valid for sequential batches", ErrInvalidSpec)
		}
	} else {
		if b.OnFail == "" {
			b.OnFail = OnFailStop
		}
		if b.OnFail != OnFailStop && b.OnFail != OnFailContinue || b.MaxParallel > 1 {
			return bad()
		}
	}
	n.view.Kind = KindBatch
	n.view.Mode = b.Mode
	n.view.OnFail = b.OnFail
	n.view.Notify = b.Notify
	n.view.MaxParallel = b.MaxParallel
	for i, child := range b.Items {
		c, err := e.build(child, ID(fmt.Sprintf("%s.%d", id, i+1)), n, i+1, depth+1, count)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
	}
	return n, nil
}

func (e *Engine) install(n *treeNode, t *coreTransition) {
	e.nodes[n.view.ID] = n
	t.created[n.view.ID] = true
	t.change(n)
	for _, c := range n.children {
		e.install(c, t)
	}
}

func (e *Engine) remove(n *treeNode) {
	delete(e.nodes, n.view.ID)
	for _, c := range n.children {
		e.remove(c)
	}
	n.task = nil
	n.children = nil
}

func (e *Engine) nodeCount(n *treeNode) int {
	total := 1
	for _, c := range n.children {
		total += e.nodeCount(c)
	}
	return total
}

func causeState(c Cause) State {
	switch c {
	case CauseStop:
		return StateCancelled
	case CauseTimeout:
		return StateTimedOut
	case CauseHostClosing:
		return StateInterrupted
	case CauseStopOnFail:
		return StateSkipped
	}
	return StateFailed
}

func (e *Engine) terminal(n *treeNode, s State, t *coreTransition) {
	n.view.State = s
	if n.task != nil {
		n.view.Control = ControlNone
		n.view.ControlPending = false
		n.view.ControlError = ""
	}
	t.settled[n.view.ID] = true
	t.change(n)
}

func (e *Engine) cancel(n *treeNode, c Cause, retry bool, t *coreTransition) {
	if n.view.State.Terminal() {
		return
	}
	fresh := n.view.Cause == CauseNone
	if fresh {
		n.view.Cause = c
		t.change(n)
	}
	if n.task == nil {
		if n.view.State == StateRunning {
			n.view.State = StateCancelling
			t.change(n)
		}
		for _, child := range n.children {
			e.cancel(child, c, retry, t)
		}
		return
	}
	if n.view.State == StatePending {
		e.terminal(n, causeState(n.view.Cause), t)
		return
	}
	if n.view.State != StateCancelling {
		n.view.State = StateCancelling
		t.change(n)
	}
	if fresh || retry && n.view.Control == ControlFailed {
		n.view.Control = ControlPending
		n.view.ControlPending = true
		t.change(n)
		t.Commands = append(t.Commands, Command{Type: CommandStopLeaf, Node: n.view.ID, Token: n.view.Token, Cause: n.view.Cause})
	}
}

func (e *Engine) settle(n *treeNode, t *coreTransition) {
	if n.task != nil || n.view.State.Terminal() {
		return
	}
	ready := true
	for i, c := range n.children {
		e.settle(c, t)
		if !c.view.State.Terminal() {
			ready = false
		}
		if c.view.State.Terminal() && n.view.Mode == ModeSequential && n.view.OnFail == OnFailStop && (c.view.State == StateFailed || c.view.State == StateTimedOut || c.view.State == StateInterrupted) {
			for _, successor := range n.children[i+1:] {
				e.cancel(successor, CauseStopOnFail, false, t)
				e.settle(successor, t)
			}
		}
	}
	if !ready {
		return
	}
	state := StateCompleted
	if n.view.Cause != CauseNone {
		state = causeState(n.view.Cause)
	} else {
		counts := e.counts(n)
		if counts.Failed+counts.TimedOut+counts.Interrupted > 0 {
			state = StateFailed
		} else if counts.Cancelled > 0 {
			state = StateCancelled
		}
	}
	e.terminal(n, state, t)
	if n.parent == nil {
		t.SettledRoots = append(t.SettledRoots, Summary{Root: n.view.ID, State: state, Counts: e.counts(n), Sequence: e.sequence})
	}
}

func (e *Engine) releaseUnit(n *treeNode) { e.active--; e.groups[n.task.Group]-- }

func (e *Engine) subtreeActive(n *treeNode) int {
	if n.task != nil {
		if n.view.State.Active() {
			return 1
		}
		return 0
	}
	total := 0
	for _, c := range n.children {
		total += e.subtreeActive(c)
	}
	return total
}

func (e *Engine) eligible(n *treeNode) bool {
	if n.task == nil || n.view.State != StatePending || n.view.Cause != CauseNone || e.closing || e.active >= e.limits.MaxConcurrent {
		return false
	}
	g := n.task.Group
	if cap, ok := e.limits.ByGroup[g]; ok && e.groups[g] >= cap {
		return false
	}
	if gen, ok := e.blocked[g]; ok && gen == e.generation[g] {
		return false
	}
	child := n
	for p := n.parent; p != nil; p = p.parent {
		if p.released || p.view.State.Terminal() || p.view.Cause != CauseNone {
			return false
		}
		if p.view.MaxParallel > 0 && e.subtreeActive(p) >= p.view.MaxParallel {
			return false
		}
		if p.view.Mode == ModeSequential {
			for _, c := range p.children {
				if !c.view.State.Terminal() {
					if c != child {
						return false
					}
					break
				}
			}
		}
		child = p
	}
	return true
}

func (e *Engine) candidate(n *treeNode) *treeNode {
	if n.released || n.view.State.Terminal() || n.view.Cause != CauseNone {
		return nil
	}
	if n.task != nil {
		if e.eligible(n) {
			return n
		}
		return nil
	}
	for _, c := range n.children {
		if leaf := e.candidate(c); leaf != nil {
			return leaf
		}
	}
	return nil
}

func (e *Engine) admit(t *coreTransition) {
	if e.closing || len(e.live) == 0 {
		return
	}
	visited := 0
	for e.active < e.limits.MaxConcurrent {
		var leaf *treeNode
		for scanned := 0; scanned < len(e.live); scanned++ {
			visited++
			index := (e.cursor + scanned) % len(e.live)
			if leaf = e.candidate(e.live[index]); leaf != nil {
				e.cursor = index + 1
				break
			}
		}
		if leaf == nil {
			break
		}
		e.token++
		leaf.view.Token = e.token
		leaf.view.Attempts++
		leaf.view.State = StateStarting
		leaf.accepted = false
		leaf.authorized = e.generation[leaf.task.Group]
		leaf.view.Control = ControlNone
		leaf.view.ControlPending = false
		leaf.view.ControlError = ""
		e.active++
		e.groups[leaf.task.Group]++
		t.change(leaf)
		for p := leaf.parent; p != nil; p = p.parent {
			if p.view.State == StatePending {
				p.view.State = StateRunning
				t.change(p)
			}
		}
		t.Commands = append(t.Commands, Command{Type: CommandStartLeaf, Node: leaf.view.ID, Token: leaf.view.Token, Launch: Launch{ID: leaf.view.ID, Token: leaf.view.Token, Task: *leaf.task}})
	}
	if e.walkInstrument != nil {
		e.walkInstrument("admit", visited)
	}
}

func (e *Engine) selectNotices(t *coreTransition) {
	var walk func(*treeNode)
	walk = func(n *treeNode) {
		for _, c := range n.children {
			walk(c)
		}
		if !t.settled[n.view.ID] {
			return
		}
		kind := n.view.Kind
		if n.parent == nil {
			kind = KindRoot
		} else {
			if n.parent.view.Notify != NotifyEach {
				return
			}
			for p := n.parent; p != nil; p = p.parent {
				if p.view.Notify == NotifyAll {
					return
				}
			}
			if len(n.parent.children) == 1 && t.settled[n.parent.view.ID] {
				return
			}
		}
		t.Notices = append(t.Notices, Notice{Root: n.view.Root, Node: n.view.ID, Kind: kind, State: n.view.State, Cause: n.view.Cause, Counts: e.counts(n)})
	}
	if e.walkInstrument != nil {
		e.walkInstrument("notices", len(e.live))
	}
	for _, root := range e.live {
		walk(root)
	}
}

// View returns a detached view of an unreleased node.
func (e *Engine) View(id ID) (NodeView, bool) {
	n, ok := e.nodes[id]
	if !ok {
		return NodeView{}, false
	}
	return n.view, true
}

// Snapshot returns an immutable pre-order snapshot of any unreleased batch.
func (e *Engine) Snapshot(id ID) (BatchView, bool) {
	n, ok := e.nodes[id]
	if !ok || n.task != nil {
		return BatchView{}, false
	}
	v := BatchView{Sequence: e.sequence, Node: n.view, Counts: e.counts(n)}
	var walk func(*treeNode)
	walk = func(p *treeNode) {
		for _, c := range p.children {
			v.Descendants = append(v.Descendants, c.view)
			walk(c)
		}
	}
	walk(n)
	return v, true
}

func (e *Engine) counts(n *treeNode) Counts {
	var counts Counts
	var walk func(*treeNode)
	walk = func(p *treeNode) {
		if p.task == nil {
			for _, c := range p.children {
				walk(c)
			}
			return
		}
		switch p.view.State {
		case StatePending:
			counts.Pending++
		case StateStarting, StateRunning, StateCancelling:
			counts.Active++
		case StateCompleted:
			counts.Completed++
		case StateFailed:
			counts.Failed++
		case StateCancelled:
			counts.Cancelled++
		case StateTimedOut:
			counts.TimedOut++
		case StateInterrupted:
			counts.Interrupted++
		case StateSkipped:
			counts.Skipped++
		}
	}
	walk(n)
	return counts
}
