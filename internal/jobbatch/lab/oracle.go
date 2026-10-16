package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

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
	_, base.controlled = ex.(*ControlledExecutor)
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
