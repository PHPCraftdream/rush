package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch/lab/controlled"
)

func NewControlledExecutor() *controlled.ControlledExecutor {
	return controlled.NewControlledExecutor()
}

var (
	ErrUnknownAttempt = controlled.ErrUnknownAttempt
	ErrAttemptPhase   = controlled.ErrAttemptPhase
)

const JournalFormat = "batchlab"

func ScenarioDigest(s *Scenario) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func journalHeader(s *Scenario) JournalHeader {
	digest, _ := ScenarioDigest(s)
	return JournalHeader{Type: "header", Format: JournalFormat, Version: 1, ScenarioSHA256: digest, Limits: s.Limits, Runner: s.Runner}
}

type journalCapture struct {
	mu          sync.Mutex
	scenario    *Scenario
	written     uint64
	writer      *json.Encoder
	sink        io.Writer
	status      JournalCapture
	records     []jobbatch.Record
	cursor      uint64
	head, final uint64
	runner      *jobbatch.Runner
	batches     map[jobbatch.ID]*jobbatch.Batch
	oracle      *Oracle
	oracleRule  string
}

func newJournalCapture(s *Scenario, w io.Writer, o *Oracle) *journalCapture {
	c := &journalCapture{scenario: s, status: JournalCapture{Status: CaptureComplete}, cursor: 1, oracle: o, sink: w}
	if w != nil {
		c.writer = json.NewEncoder(w)
		if c.writer.Encode(journalHeader(s)) != nil {
			c.status.Status = CaptureWriteError
		}
	}
	return c
}

func (c *journalCapture) page(p jobbatch.EventPage, err error, cut uint64) {
	c.head, c.final = p.Head, p.Final
	if errors.Is(err, jobbatch.ErrGap) {
		if c.status.Status == CaptureComplete {
			first, resume := c.cursor, p.Oldest
			c.status = JournalCapture{Status: CaptureGap, FirstMissing: &first, ResumedAt: &resume}
		}
		c.cursor = p.Oldest
		return
	}
	for _, r := range p.Records {
		if cut != 0 && r.Cursor > cut {
			break
		}
		if r.Cursor < c.cursor {
			continue
		}
		c.records = append(c.records, r)
		c.oracle.record(r)
		if c.writer != nil && c.status.Status != CaptureWriteError {
			if c.writer.Encode(r) != nil {
				c.status = JournalCapture{Status: CaptureWriteError}
			} else {
				c.written = r.Cursor
			}
		}
		c.cursor = r.Cursor + 1
	}
}

func (c *journalCapture) follow(ctx context.Context, r *jobbatch.Runner) {
	for {
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		cursor := c.cursor
		c.mu.Unlock()
		p, err := r.Events(ctx, cursor)
		c.mu.Lock()
		if ctx.Err() != nil {
			c.mu.Unlock()
			return // Cancellation freezes capture before a bounded reader join.
		}
		c.page(p, err, 0)
		c.mu.Unlock()
		if err != nil && !errors.Is(err, jobbatch.ErrGap) {
			return
		}
		if p.Final != 0 && c.cursor > p.Final {
			return
		}
	}
}

func (c *journalCapture) drain(ctx context.Context, r *jobbatch.Runner) {
	c.runner = r
	// A cancelled probe reads available records without waiting; its Head/Final
	// fixes this drain boundary. Later completions must not enlarge a pre-F cut.
	probe, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := r.Events(probe, c.cursor)
	cut, final := p.Head, p.Final
	c.page(p, err, cut)
	for c.cursor <= cut {
		readCtx := ctx
		if final == 0 {
			readCtx = probe
		}
		p, err = r.Events(readCtx, c.cursor)
		c.page(p, err, cut)
		if err != nil && !errors.Is(err, jobbatch.ErrGap) {
			if c.cursor <= cut && c.status.Status == CaptureComplete {
				c.status.Status = CaptureWriteError
			}
			break
		}
	}
	c.head, c.final = cut, final
}

func (c *journalCapture) footer(s *Scenario, inputs []JournalInput, counts RequestCounts) JournalFooter {
	f := JournalFooter{Type: "footer", Roots: []JournalRoot{}, Capture: c.status, Inputs: inputs, RequestCounts: counts, Outcome: JournalOutcome{Status: OutcomeUnfinished}}
	if inputs == nil {
		inputs = []JournalInput{}
		f.Inputs = inputs
	}
	if len(c.records) > 0 {
		last := c.records[len(c.records)-1]
		f.LastCursor = last.Cursor
		f.FinalSequence = last.Sequence
	}
	if c.final != 0 {
		f.FinalCursor = &c.final
		f.Outcome.Status = OutcomeClosed
	}
	if c.writer != nil {
		f.LastCursor = c.written
	}
	state, err := journalState(s, c.records)
	if err == nil {
		f.Roots = journalSortedRoots(state.roots)
		if f.Outcome.Status == OutcomeUnfinished {
			ids := journalUnfinished(state, inputs)
			f.Outcome.IDs = &ids
		}
	} else {
		// Lost history is not reconstructed from snapshots.
		if c.final != 0 {
			probe, cancel := context.WithCancel(context.Background())
			cancel()
			for _, in := range inputs {
				if in.Action != ActionSubmit || in.Result != InputAccepted || in.IDs == nil {
					continue
				}
				id := (*in.IDs)[0]
				if view, err := c.runner.Snapshot(probe, id); err == nil {
					jr := JournalRoot{ID: id, State: view.Node.State, Counts: view.Counts}
					for _, r := range c.records {
						if r.Type == jobbatch.RecordNotice && r.Node == id {
							seq := r.Sequence
							jr.SettledSequence = &seq
						}
					}
					if batch := c.batches[id]; batch != nil {
						if summary, err := batch.Wait(probe); err == nil {
							jr.State, jr.Counts = summary.State, summary.Counts
							seq := summary.Sequence
							jr.SettledSequence = &seq
						}
					}
					f.Roots = append(f.Roots, jr)
				}
			}
		}
		if f.Outcome.Status == OutcomeUnfinished {
			ids := []jobbatch.ID{}
			f.Outcome.IDs = &ids
		}
	}
	if f.FinalCursor != nil && (len(c.records) == 0 || c.records[len(c.records)-1].Type != jobbatch.RecordClosed || f.LastCursor != *f.FinalCursor) && f.Capture.Status == CaptureComplete {
		f.Capture.Status = CaptureWriteError
		c.status = f.Capture
	}
	return f
}

func (c *journalCapture) writeFooter(f JournalFooter) {
	if c.sink != nil && json.NewEncoder(c.sink).Encode(f) != nil {
		c.status = JournalCapture{Status: CaptureWriteError}
	}
}

func journalSortedRoots(m map[jobbatch.ID]JournalRoot) []JournalRoot {
	keys := make([]string, 0, len(m))
	for id := range m {
		keys = append(keys, string(id))
	}
	sort.Strings(keys)
	out := make([]JournalRoot, 0, len(keys))
	for _, id := range keys {
		out = append(out, m[jobbatch.ID(id)])
	}
	return out
}

// Unfinished identities come from the captured Engine prefix, descendants
// before their root, including nodes accepted by later Add requests.
func journalUnfinished(state journalDerived, inputs []JournalInput) []jobbatch.ID {
	ids := []jobbatch.ID{}
	var visit func(jobbatch.ID)
	visit = func(parent jobbatch.ID) {
		children := []jobbatch.NodeView{}
		for _, v := range state.nodes {
			if v.Parent == parent {
				children = append(children, v)
			}
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Ordinal < children[j].Ordinal })
		for _, v := range children {
			visit(v.ID)
			if !v.State.Terminal() {
				ids = append(ids, v.ID)
			}
		}
	}
	for _, in := range inputs {
		if in.Action != ActionSubmit || in.Result != InputAccepted || in.IDs == nil || len(*in.IDs) != 1 {
			continue
		}
		root := (*in.IDs)[0]
		visit(root)
		if v, ok := state.nodes[root]; ok && !v.State.Terminal() {
			ids = append(ids, root)
		}
	}
	return ids
}

// Oracle observes physical calls, never admission or scheduling. Labels remain
// available to public-call harnesses after the corresponding call returned.
type Oracle struct {
	mu         sync.Mutex
	changed    chan struct{}
	clock      uint64
	sealed     bool
	physical   int
	reads      int
	labels     map[string]uint64
	aliases    map[string]uint64
	attempts   map[uint64]*oracleAttempt
	finalProbe func() jobbatch.EventPage
}
type oracleAttempt struct {
	node            jobbatch.ID
	entry, end      uint64
	accepted        bool
	stops, disposes int
}

func NewOracle() *Oracle {
	return &Oracle{changed: make(chan struct{}), labels: map[string]uint64{}, aliases: map[string]uint64{}, attempts: map[uint64]*oracleAttempt{}}
}

func (o *Oracle) tick() uint64 {
	o.clock++
	close(o.changed)
	o.changed = make(chan struct{})
	return o.clock
}

func (o *Oracle) label(s string) {
	if o.sealed {
		return
	}
	if _, ok := o.labels[s]; !ok {
		o.labels[s] = o.tick()
	}
}

func (o *Oracle) Bind(name string, token uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.sealed {
		o.aliases[name] = token
		o.tick()
	}
}

func (o *Oracle) labelTime(s string) uint64 {
	if n := o.labels[s]; n != 0 {
		return n
	}
	for name, t := range o.aliases {
		prefix := name + ":"
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return o.labels[fmt.Sprintf("#%d:%s", t, s[len(prefix):])]
		}
	}
	return 0
}

func (o *Oracle) AwaitLabel(ctx context.Context, label string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.labelTime(label) == 0 {
		ch := o.changed
		o.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			o.mu.Lock()
			return ctx.Err()
		}
		o.mu.Lock()
	}
	return nil
}

func (o *Oracle) observe(token uint64, kind string, call uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := fmt.Sprintf("#%d:%s", token, kind)
	if call != 0 {
		s += fmt.Sprintf("#%d", call)
	}
	o.label(s)
}

func (o *Oracle) record(r jobbatch.Record) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r.Type == jobbatch.RecordClosed {
		o.label("closed")
	}
	if r.Node != "" {
		o.label(string(r.Node) + ":" + string(r.Type))
	}
}

func (o *Oracle) maximum() int {
	max := 0
	for _, a := range o.attempts {
		if !a.accepted {
			continue
		}
		n := 0
		for _, b := range o.attempts {
			if b.accepted && b.entry <= a.entry && (b.end == 0 || b.end > a.entry) {
				n++
			}
		}
		if n > max {
			max = n
		}
	}
	return max
}

func (o *Oracle) counters(id jobbatch.ID) (starts, accepted, stops, disposes int) {
	for _, a := range o.attempts {
		if a.node == id {
			starts++
			if a.accepted {
				accepted++
			}
			stops += a.stops
			disposes += a.disposes
		}
	}
	return
}

type oracleExecutor struct {
	oracle *Oracle
	routes map[string]jobbatch.Executor
}

func (e *oracleExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	e.oracle.beginPhysical()
	defer e.oracle.endPhysical()
	o := e.oracle
	o.mu.Lock()
	a := &oracleAttempt{node: l.ID}
	if !o.sealed {
		a.entry = o.tick()
		o.attempts[l.Token] = a
	}
	o.mu.Unlock()
	ex := e.routes[l.Task.Kind]
	if ex == nil {
		return nil, jobbatch.ErrUnsupported
	}
	execution, err := ex.Start(ctx, l, func(r jobbatch.Result) {
		o.mu.Lock()
		if !o.sealed && a.end == 0 {
			a.end = o.tick()
		}
		o.mu.Unlock()
		report(r)
	})
	if err != nil || execution == nil {
		return execution, err
	}
	o.mu.Lock()
	if !o.sealed {
		a.accepted = true
		o.tick()
	}
	o.mu.Unlock()
	base := &oracleExecution{Execution: execution, o: o, token: l.Token}
	_, base.controlled = ex.(*controlled.ControlledExecutor)
	return oracleCapabilities(base), nil
}

type oracleExecution struct {
	jobbatch.Execution
	o          *Oracle
	controlled bool
	token      uint64
}

func (e *oracleExecution) Stop(ctx context.Context) error {
	e.o.beginPhysical()
	defer e.o.endPhysical()
	e.o.mu.Lock()
	k := uint64(0)
	if a := e.o.attempts[e.token]; !e.o.sealed && a != nil {
		a.stops++
		k = uint64(a.stops)
	}
	e.o.mu.Unlock()
	e.o.observe(e.token, "stop_entered", k)
	defer e.o.observe(e.token, "stop_returned", k)
	return e.Execution.Stop(ctx)
}

type oracleOutput struct{ e *oracleExecution }

func (x oracleOutput) ReadOutput(ctx context.Context, c uint64) (jobbatch.OutputChunk, error) {
	x.e.o.beginPhysical()
	x.e.o.mu.Lock()
	x.e.o.reads++
	x.e.o.mu.Unlock()
	defer func() {
		x.e.o.mu.Lock()
		x.e.o.reads--
		x.e.o.mu.Unlock()
		x.e.o.endPhysical()
	}()
	if !x.e.controlled {
		x.e.o.observe(x.e.token, "read_entered", 0)
		defer x.e.o.observe(x.e.token, "read_returned", 0)
	}
	return x.e.Execution.(jobbatch.OutputReader).ReadOutput(ctx, c)
}

type oracleMessage struct{ e *oracleExecution }

func (x oracleMessage) Inject(ctx context.Context, m json.RawMessage) error {
	return x.e.Execution.(jobbatch.Messenger).Inject(ctx, m)
}

type oracleDispose struct{ e *oracleExecution }

func (x oracleDispose) Dispose(ctx context.Context) error {
	x.e.o.beginPhysical()
	defer x.e.o.endPhysical()
	// Events on a cancelled context is a nonblocking producer boundary probe.
	// Final, not reader scheduling, establishes Closed before this physical call.
	if probe := x.e.o.finalProbe; probe != nil {
		p := probe()
		if p.Final != 0 {
			// Final alone proves the producer boundary. Replaying every retained
			// record here adds observer work to the delegate's control deadline.
			x.e.o.mu.Lock()
			x.e.o.label("closed")
			x.e.o.mu.Unlock()
		}
	}
	x.e.o.mu.Lock()
	if a := x.e.o.attempts[x.e.token]; !x.e.o.sealed && a != nil {
		a.disposes++
	}
	x.e.o.mu.Unlock()
	x.e.o.observe(x.e.token, "dispose_entered", 0)
	defer x.e.o.observe(x.e.token, "dispose_returned", 0)
	return x.e.Execution.(jobbatch.Disposer).Dispose(ctx)
}

// Each combination exposes exactly the delegate's optional interfaces.
func oracleCapabilities(e *oracleExecution) jobbatch.Execution {
	_, r := e.Execution.(jobbatch.OutputReader)
	_, m := e.Execution.(jobbatch.Messenger)
	_, d := e.Execution.(jobbatch.Disposer)
	switch {
	case r && m && d:
		return struct {
			jobbatch.Execution
			jobbatch.OutputReader
			jobbatch.Messenger
			jobbatch.Disposer
		}{e, oracleOutput{e}, oracleMessage{e}, oracleDispose{e}}
	case r && m:
		return struct {
			jobbatch.Execution
			jobbatch.OutputReader
			jobbatch.Messenger
		}{e, oracleOutput{e}, oracleMessage{e}}
	case r && d:
		return struct {
			jobbatch.Execution
			jobbatch.OutputReader
			jobbatch.Disposer
		}{e, oracleOutput{e}, oracleDispose{e}}
	case m && d:
		return struct {
			jobbatch.Execution
			jobbatch.Messenger
			jobbatch.Disposer
		}{e, oracleMessage{e}, oracleDispose{e}}
	case r:
		return struct {
			jobbatch.Execution
			jobbatch.OutputReader
		}{e, oracleOutput{e}}
	case m:
		return struct {
			jobbatch.Execution
			jobbatch.Messenger
		}{e, oracleMessage{e}}
	case d:
		return struct {
			jobbatch.Execution
			jobbatch.Disposer
		}{e, oracleDispose{e}}
	default:
		return e
	}
}

func oracleEqual[T comparable](p *T, v T) bool { return p == nil || *p == v }
func (c *journalCapture) checkOracle(expect *Expect, o *Oracle, replay bool) bool {
	if expect == nil {
		return true
	}
	c.oracleRule = "journal state"
	state, err := journalState(c.scenario, c.records)
	if err != nil || !replay && c.status.Status == CaptureGap {
		// Final views may verify state, never repair missing feed or notices
		// (§4.1/5.5). Replay still cannot reconstruct incomplete history.
		if replay || c.final == 0 || c.runner == nil {
			return false
		}
		state = journalDerived{nodes: map[jobbatch.ID]jobbatch.NodeView{}, roots: map[jobbatch.ID]JournalRoot{}}
		probe, cancel := context.WithCancel(context.Background())
		cancel()
		for id := range c.batches {
			view, err := c.runner.Snapshot(probe, id)
			if err != nil {
				return false
			}
			state.roots[id] = JournalRoot{ID: id, State: view.Node.State, Counts: view.Counts}
			state.nodes[id] = view.Node
			for _, v := range view.Descendants {
				state.nodes[v.ID] = v
			}
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c.oracleRule = "roots"
	if expect.Roots != nil {
		if len(*expect.Roots) != len(state.roots) {
			return false
		}
		for _, want := range *expect.Roots {
			got, ok := state.roots[want.ID]
			if !ok || !oracleEqual(want.State, got.State) || !oracleEqual(want.Counts, got.Counts) {
				return false
			}
		}
	}
	c.oracleRule = "nodes"
	if expect.Nodes != nil {
		for _, want := range *expect.Nodes {
			got, ok := state.nodes[want.ID]
			if !ok {
				return false
			}
			control := string(got.Control)
			if control == "" {
				control = "none"
			}
			if got.Control == jobbatch.ControlFailed {
				control += "(" + got.ControlError + ")"
			}
			if !oracleEqual(want.Control, control) {
				c.oracleRule = "node control: actual " + control
				return false
			}
			if !oracleEqual(want.Parent, got.Parent) || !oracleEqual(want.State, got.State) || !oracleEqual(want.Cause, got.Cause) || !oracleEqual(want.Control, control) || !oracleEqual(want.ControlPending, got.ControlPending) || !oracleEqual(want.Attempts, got.Attempts) {
				return false
			}
			if want.Outcome != nil && (!oracleEqual(want.Outcome.State, got.Outcome.State) || !oracleEqual(want.Outcome.Code, got.Outcome.Code)) {
				return false
			}
			if !replay {
				starts, accepted, stops, disposes := o.counters(want.ID)
				if !oracleEqual(want.Starts, starts) || !oracleEqual(want.Accepted, accepted) || !oracleEqual(want.Stops, stops) || !oracleEqual(want.Disposes, disposes) {
					return false
				}
			}
		}
	}
	c.oracleRule = "max_active"
	if !replay && !oracleEqual(expect.MaxActive, o.maximum()) {
		return false
	}
	c.oracleRule = "notices"
	if expect.Notices != nil {
		actual := map[jobbatch.ID][]jobbatch.Record{}
		for _, r := range c.records {
			if r.Type == jobbatch.RecordNotice {
				actual[r.Root] = append(actual[r.Root], r)
			}
		}
		if len(actual) != len(*expect.Notices) {
			return false
		}
		for root, wants := range *expect.Notices {
			got := actual[root]
			if len(got) != len(wants) {
				return false
			}
			for i, want := range wants {
				r := got[i]
				if r.Node != want.Node || !oracleEqual(want.State, r.State) || !oracleEqual(want.Cause, r.Cause) || want.Counts != nil && (r.Counts == nil || !reflect.DeepEqual(*want.Counts, *r.Counts)) {
					return false
				}
			}
		}
	}
	c.oracleRule = "before"
	if expect.Before != nil {
		for _, pair := range *expect.Before {
			// Replay checks only feed-expressible labels. Physical call labels are not
			// history and cannot be recovered from a journal.
			left, right := o.labelTime(pair[0]), o.labelTime(pair[1])
			if !oraclePhysicalLabel(pair[0]) && !oraclePhysicalLabel(pair[1]) {
				left, right = oracleFeedCursor(c.records, pair[0]), oracleFeedCursor(c.records, pair[1])
			}
			if replay && (oraclePhysicalLabel(pair[0]) || oraclePhysicalLabel(pair[1])) {
				continue
			}
			if left == 0 || right == 0 || left >= right {
				return false
			}
		}
	}
	c.oracleRule = ""
	return true
}

func oracleFeedCursor(records []jobbatch.Record, label string) uint64 {
	for _, r := range records {
		if r.Type == jobbatch.RecordClosed && label == "closed" || r.Node != "" && string(r.Node)+":"+string(r.Type) == label {
			return r.Cursor
		}
	}
	return 0
}

func oraclePhysicalLabel(s string) bool {
	for _, suffix := range []string{":stop_entered", ":stop_returned", ":dispose_entered", ":dispose_returned", ":read_entered", ":read_returned"} {
		for i := 0; i+len(suffix) <= len(s); i++ {
			if s[i:i+len(suffix)] == suffix {
				return true
			}
		}
	}
	return false
}

type journalDerived struct {
	nodes    map[jobbatch.ID]jobbatch.NodeView
	roots    map[jobbatch.ID]JournalRoot
	sequence uint64
}

func journalState(s *Scenario, records []jobbatch.Record) (journalDerived, error) {
	state := journalDerived{nodes: map[jobbatch.ID]jobbatch.NodeView{}, roots: map[jobbatch.ID]JournalRoot{}}
	e, err := jobbatch.NewEngine(s.Limits)
	if err != nil {
		return state, err
	}
	refs := journalActionMap(s)
	for _, r := range records {
		if r.Type == jobbatch.RecordCommand || r.Type == jobbatch.RecordNotice || r.Type == jobbatch.RecordClosed {
			continue
		}
		ev, err := journalEvent(s, r, refs)
		if err != nil {
			return state, err
		}
		t, err := e.Apply(ev)
		if err != nil {
			return state, err
		}
		state.sequence = t.Sequence
		for _, v := range t.Changed {
			state.nodes[v.ID] = v
			if v.Parent == "" {
				if _, ok := state.roots[v.ID]; !ok {
					state.roots[v.ID] = JournalRoot{ID: v.ID}
				}
			}
		}
		for id, root := range state.roots {
			if view, ok := e.Snapshot(id); ok {
				root.State, root.Counts = view.Node.State, view.Counts
				state.nodes[id] = view.Node
				for _, v := range view.Descendants {
					state.nodes[v.ID] = v
				}
				state.roots[id] = root
			}
		}
		for _, summary := range t.SettledRoots {
			root := state.roots[summary.Root]
			root.State, root.Counts = summary.State, summary.Counts
			seq := summary.Sequence
			root.SettledSequence = &seq
			state.roots[summary.Root] = root
		}
	}
	return state, nil
}

// Physical accounting never authorizes or rejects a delegate call. Sealing
// freezes measurements only; callbacks and later cleanup still reach delegates.
func (o *Oracle) beginPhysical() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.physical++
	if !o.sealed {
		o.tick()
	}
}

func (o *Oracle) endPhysical() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.physical--
	if !o.sealed {
		o.tick()
	}
}

// Only abandoned output needs a physical join. Stop/Dispose logical timeouts
// are authoritative; held controls must remain script-owned, not spend another
// cleanup budget waiting for their physical returns.
func (o *Oracle) finishPhysical(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.reads != 0 {
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			o.mu.Lock()
			o.sealed = true
			return ctx.Err()
		}
		o.mu.Lock()
	}
	o.sealed = true
	return nil
}
