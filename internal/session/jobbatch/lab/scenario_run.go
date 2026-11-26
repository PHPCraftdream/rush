package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch/lab/controlled"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch/lab/process"
)

// RunOptions supplies bounded cleanup and public harness observation seams.
// OpenEvents is called only after the whole scenario has validated. Async
// close/output and reader hooks have budgeted joins: a hook ignoring cancellation
// yields ExitUnfinished and may outlive the measurement cut. Synchronous hooks
// and writer methods must honor their own bounded-return contract.
type RunOptions struct {
	OpenEvents        func() (io.WriteCloser, error)
	ProcessOptions    process.ProcessOptions
	CleanupBound      time.Duration
	Oracle            *Oracle
	BeforeAction      func(context.Context, int, *RunHarness) error
	BeforeEventsRead  func(context.Context) error
	BeforeCloseReturn func(context.Context) error
}
type (
	RunHarness struct {
		Runner     *jobbatch.Runner
		Controlled *controlled.ControlledExecutor
		Oracle     *Oracle
	}
	RunResult struct {
		ExitCode      int
		Footer        JournalFooter
		Mismatches    []int
		OracleRule    string
		CloseFailures []ScenarioCloseFailure
	}
	ScenarioCloseFailure struct {
		Action   int
		Error    ErrorName
		Failures []jobbatch.DisposeFailure
		Physical []controlled.ScenarioPhysicalState
	}

	scenarioRun struct {
		s           *Scenario
		r           *jobbatch.Runner
		c           *controlled.ControlledExecutor
		o           *Oracle
		bound       context.Context
		bindings    map[string]uint64
		async       map[string]scenarioAsync
		inputs      []JournalInput
		roots       map[jobbatch.ID]*jobbatch.Batch
		closeReturn func(context.Context) error
		counts      RequestCounts
	}
)

type scenarioAsync struct {
	action Action
	done   chan error
}

// RunScenario interprets actions against one Runner for the entire forest.
func RunScenario(ctx context.Context, s *Scenario, opts RunOptions) RunResult {
	result := RunResult{ExitCode: ExitInvalid}
	if ValidateScenario(s) != nil {
		return result
	}
	cleanup := opts.CleanupBound
	if cleanup <= 0 {
		cleanup = 5 * time.Second
	}
	o := opts.Oracle
	if o == nil {
		o = NewOracle()
	}
	c := controlled.NewControlledExecutor()
	c.SetObserveHook(func(obs controlled.Observation) { o.observe(obs.Token, obs.Kind, obs.Call) })
	routes := map[string]jobbatch.Executor{"controlled": c}
	for _, p := range s.Payloads {
		if p.Kind == "process" {
			po := opts.ProcessOptions
			if po.MaxRetainedBytes <= 0 {
				po.MaxRetainedBytes = 1 << 20
			}
			ex, err := process.NewProcessExecutor(po)
			if err != nil {
				return result
			}
			routes["process"] = ex
			break
		}
	}
	var writer io.WriteCloser
	if opts.OpenEvents != nil {
		var err error
		writer, err = opts.OpenEvents()
		if err != nil {
			return result
		}
		defer writer.Close()
	}
	host, cancelHost := context.WithCancel(context.Background())
	defer cancelHost()
	r, err := jobbatch.NewRunner(host, jobbatch.RunnerConfig{Limits: s.Limits, Executor: &oracleExecutor{o, routes}, ControlTimeout: time.Duration(s.Runner.ControlTimeoutMS) * time.Millisecond, RecordRetention: s.Runner.RecordRetention})
	if err != nil {
		return result
	}
	o.finalProbe = func() jobbatch.EventPage {
		probe, stop := context.WithCancel(context.Background())
		stop()
		p, _ := r.Events(probe, ^uint64(0))
		return p
	}
	bound, cancel := context.WithTimeout(ctx, time.Duration(s.WaitBoundMS)*time.Millisecond)
	defer cancel()
	run := &scenarioRun{roots: map[jobbatch.ID]*jobbatch.Batch{}, closeReturn: opts.BeforeCloseReturn, s: s, r: r, c: c, o: o, bound: bound, bindings: map[string]uint64{}, async: map[string]scenarioAsync{}}
	capture := newJournalCapture(s, writer, o)
	capture.batches = run.roots
	readerCtx, readerCancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		if opts.BeforeEventsRead != nil {
			if opts.BeforeEventsRead(readerCtx) != nil {
				return
			}
		}
		if readerCtx.Err() != nil {
			return // A late external hook must not touch the frozen capture.
		}
		capture.follow(readerCtx, r)
	}()
	unfinished, mismatch := false, false
	closeAttempted, asyncOutput := false, false
	harness := &RunHarness{r, c, o}
	// A rejected return_stop released no held call, so no physical return follows.
	var stopFence *Action
	for i, a := range s.Script {
		if bound.Err() != nil {
			unfinished = true
			break
		}
		if previous := stopFence; previous != nil {
			stopFence = nil
			// §6.6: gate release is not physical return. Fence the next
			// dependent action, without changing ReturnStop's contract.
			if err := o.AwaitLabel(bound, fmt.Sprintf("%s:stop_returned#%d", *previous.Attempt, *previous.Call)); err != nil {
				unfinished = true
				break
			}
		}
		if opts.BeforeAction != nil {
			if err := opts.BeforeAction(bound, i, harness); err != nil {
				mismatch = true
				result.Mismatches = append(result.Mismatches, i)
				continue
			}
		}
		if a.Async != nil {
			asyncOutput = asyncOutput || a.Action == ActionExpectOutput
			done := make(chan error, 1)
			run.async[*a.Async] = scenarioAsync{a, done}
			go func(a Action) { done <- run.action(bound, a) }(a)
			continue
		}
		var actionErr error
		checked := a
		if a.Action == ActionJoin {
			task := run.async[*a.Name]
			checked = task.action
			joinCtx, stop := run.actionContext(bound, a)
			select {
			case actionErr = <-task.done:
				delete(run.async, *a.Name)
			case <-joinCtx.Done():
				actionErr = joinCtx.Err()
			}
			stop()
		} else {
			actionErr = run.action(bound, a)
		}
		if checked.Action == ActionClose {
			closeAttempted = true
			if actionErr != nil {
				info := ScenarioCloseFailure{Action: i, Error: ErrorNameOf(actionErr)}
				var dispose *jobbatch.DisposeError
				if errors.As(actionErr, &dispose) {
					info.Failures = append([]jobbatch.DisposeFailure(nil), dispose.Failures...)
				}
				info.Physical = c.ScenarioPhysicalState()
				result.CloseFailures = append(result.CloseFailures, info)
			}
		}
		if errors.Is(actionErr, jobbatch.ErrCloseTimeout) || errors.Is(actionErr, jobbatch.ErrDispose) || (errors.Is(actionErr, context.Canceled) || errors.Is(actionErr, context.DeadlineExceeded)) && checked.ExpectError == nil {
			unfinished = true
		}
		if !scenarioActionMatches(checked, actionErr) {
			mismatch = true
			result.Mismatches = append(result.Mismatches, i)
		}
		if a.Action == ActionReturnStop && actionErr == nil {
			stopFence = &a
		}
	}
	boundExpired := bound.Err() != nil
	if boundExpired {
		unfinished = true
	}
	cleanupCtx, cleanupStop := context.WithTimeout(context.Background(), cleanup)
	defer cleanupStop()
	cancel() // Cancel outstanding async public calls before bounded joining.
	if asyncOutput {
		// Output may already have returned logically on cancellation while its
		// underlying held read is still unwinding. Never release control gates.
		o.mu.Lock()
		held := o.reads != 0
		o.mu.Unlock()
		if held {
			c.ReleaseCleanupReads()
		}
	}
	for _, task := range run.async {
		select {
		case err := <-task.done:
			if !scenarioActionMatches(task.action, err) {
				mismatch = true
			}
		case <-cleanupCtx.Done():
			// External hooks may ignore context. Async close/output never write
			// harness maps or inputs, so a bounded cut remains safe.
		}
		unfinished = true // An abandoned join is not script completion.
	}
	if !closeAttempted || boundExpired {
		closeCtx := cleanupCtx
		if err := r.Close(closeCtx); err != nil {
			unfinished = true
		}
	}
	readerCancel()
	select {
	case <-readerDone:
	case <-cleanupCtx.Done():
		// A stuck external reader hook cannot enter follow after cancellation.
		unfinished = true
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.drain(cleanupCtx, r)
	cancelHost()
	if err := o.finishPhysical(cleanupCtx); err != nil {
		unfinished = true
	}
	result.Footer = capture.footer(s, run.inputs, run.counts)
	if writer != nil {
		capture.writeFooter(result.Footer)
		result.Footer.Capture = capture.status
	}
	code := ExitSuccess
	for _, root := range result.Footer.Roots {
		if root.State != jobbatch.StateCompleted {
			code = ExitFailed
		}
	}
	if !capture.checkOracle(s.Expect, o, false) {
		result.OracleRule = capture.oracleRule
		mismatch = true
	}
	if mismatch {
		code = ExitMismatch
	}
	if writer != nil && result.Footer.Capture.Status != CaptureComplete {
		code = ExitIncompleteJournal
	}
	if unfinished || result.Footer.Outcome.Status == OutcomeUnfinished {
		code = ExitUnfinished
	}
	if s.Expect != nil && s.Expect.ExitCode != nil && *s.Expect.ExitCode != code && code != ExitUnfinished && code != ExitIncompleteJournal {
		code = ExitMismatch
	}
	result.ExitCode = code
	return result
}

func scenarioActionMatches(a Action, err error) bool {
	if a.ExpectError != nil {
		return err != nil && ErrorNameOf(err) == *a.ExpectError
	}
	if a.Action == ActionClose && a.Expect != nil {
		switch *a.Expect {
		case CloseTimeout:
			return errors.Is(err, jobbatch.ErrCloseTimeout)
		case CloseDispose:
			return errors.Is(err, jobbatch.ErrDispose)
		case CloseOK:
			return err == nil
		}
	}
	return err == nil
}

func (run *scenarioRun) actionContext(ctx context.Context, a Action) (context.Context, context.CancelFunc) {
	if a.TimeoutMS != nil {
		return context.WithTimeout(ctx, time.Duration(*a.TimeoutMS)*time.Millisecond)
	}
	return context.WithCancel(ctx)
}

var errScenarioMismatch = errors.New("batchlab: expectation mismatch")

func scenarioValue[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return
}

func (run *scenarioRun) action(parent context.Context, a Action) error {
	ctx, cancel := run.actionContext(parent, a)
	defer cancel()
	token := uint64(0)
	// Only async close/output exist; neither uses an attempt binding.
	if a.Attempt != nil {
		token = run.bindings[*a.Attempt]
	}
	switch a.Action {
	case ActionSubmit:
		req, err := run.s.SubmitRequest(a)
		if err != nil {
			return err
		}
		b, err := run.r.Submit(ctx, req)
		var ids []jobbatch.ID
		if err == nil {
			ids = []jobbatch.ID{b.ID()}
			run.roots[b.ID()] = b
		}
		run.input(a, ids, err)
		return err
	case ActionAdd:
		req, err := run.s.AddRequest(a)
		if err != nil {
			return err
		}
		ids, err := run.r.Add(ctx, req)
		run.input(a, ids, err)
		return err
	case ActionStop:
		return run.r.Stop(ctx, *a.Node)
	case ActionInject:
		msg, err := run.s.ResolveMessage(*a.MessageRef)
		if err != nil {
			return err
		}
		return run.r.Inject(ctx, *a.Node, msg)
	case ActionCapacityAvailable:
		err := run.r.CapacityAvailable(ctx, *a.Group)
		if err == nil {
			run.counts.CapacityAvailable++
		}
		return err
	case ActionRelease:
		err := run.r.Release(ctx, *a.Root)
		if err == nil {
			run.counts.Release++
		}
		return err
	case ActionClose:
		err := run.r.Close(ctx)
		if run.closeReturn != nil {
			if gateErr := run.closeReturn(ctx); gateErr != nil && err == nil {
				return gateErr
			}
		}
		return err
	case ActionAwaitStart:
		l, err := run.c.AwaitStart(ctx, *a.Node)
		if err == nil {
			run.bindings[*a.Bind] = l.Token
			run.o.Bind(*a.Bind, l.Token)
		}
		return err
	case ActionReleaseStart:
		return run.c.ReleaseStart(ctx, token, controlled.StartDecision{Decision: *a.Decision, State: scenarioValue(a.State), Code: scenarioValue(a.Code)})
	case ActionComplete:
		return run.c.Complete(ctx, token, jobbatch.Result{State: *a.State, Code: scenarioValue(a.Code)})
	case ActionEmit:
		return run.c.Emit(token, []byte(*a.Text))
	case ActionAwaitInject:
		msg, err := run.c.AwaitInject(ctx, token)
		if err != nil {
			return err
		}
		expected, err := run.s.ResolveMessage(*a.MessageRef)
		if err != nil {
			return err
		}
		if !bytes.Equal(msg, expected) {
			return errScenarioMismatch
		}
		return nil
	case ActionHoldStop:
		return run.c.HoldStop(token)
	case ActionFailStop:
		return run.c.FailNextStop(token, *a.Code)
	case ActionAwaitStop:
		return run.c.AwaitStop(ctx, token, *a.Call)
	case ActionReturnStop:
		return run.c.ReturnStop(token, *a.Call, *a.Result)
	case ActionHoldDispose:
		return run.c.HoldDispose(token)
	case ActionAwaitDispose:
		return run.c.AwaitDispose(ctx, token)
	case ActionReturnDispose:
		return run.c.ReturnDispose(token, *a.Result)
	case ActionHoldRead:
		return run.c.HoldRead(token)
	case ActionReturnRead:
		return run.c.ReturnRead(token)
	case ActionAwaitState:
		return run.awaitState(ctx, *a.Node, *a.State)
	case ActionAwaitRecord:
		return run.awaitRecord(ctx, a)
	case ActionExpectOutput:
		return run.expectOutput(ctx, a)
	}
	return scenarioInvalid("unsupported action")
}

func (run *scenarioRun) input(a Action, ids []jobbatch.ID, err error) {
	in := JournalInput{Ref: *a.Ref, Action: a.Action, Result: InputAccepted}
	if err == nil {
		if ids == nil {
			ids = []jobbatch.ID{}
		}
		in.IDs = &ids
	} else {
		in.Result = InputRejected
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			in.Result = InputAbandoned
		}
		name := ErrorNameOf(err)
		in.Error = &name
	}
	run.inputs = append(run.inputs, in)
}

func (run *scenarioRun) awaitRecord(ctx context.Context, a Action) error {
	cursor := uint64(1)
	for {
		p, err := run.r.Events(ctx, cursor)
		if err != nil {
			return err
		}
		for _, r := range p.Records {
			if r.Type == *a.Type && (a.Node == nil || r.Node == *a.Node) && (a.Code == nil || r.Code == *a.Code) {
				run.o.record(r)
				return nil
			}
		}
		cursor = p.Next
	}
}

func (run *scenarioRun) awaitState(ctx context.Context, id jobbatch.ID, state jobbatch.State) error {
	// Read Head before View: a change between those reads is either visible to
	// View or wakes Events(Head+1). No wall-clock polling and no lost wake-up.
	cursor := uint64(1)
	for {
		probe, cancel := context.WithCancel(context.Background())
		cancel()
		p, err := run.r.Events(probe, cursor)
		if err != nil && !errors.Is(err, jobbatch.ErrGap) && !errors.Is(err, jobbatch.ErrClosed) && !errors.Is(err, context.Canceled) {
			return err
		}
		cursor = p.Head + 1
		v, err := run.r.View(ctx, id)
		if err != nil {
			return err
		}
		if v.State == state || state == "terminal" && v.State.Terminal() {
			return nil
		}
		if p.Final != 0 {
			return errScenarioMismatch
		}
		p, err = run.r.Events(ctx, cursor)
		if err != nil && !errors.Is(err, jobbatch.ErrGap) {
			return err
		}
		if errors.Is(err, jobbatch.ErrGap) {
			cursor = p.Oldest
		} else {
			cursor = p.Next
		}
	}
}

func (run *scenarioRun) expectOutput(ctx context.Context, a Action) error {
	cursor := uint64(0)
	var text strings.Builder
	for {
		chunk, err := run.r.Output(ctx, *a.Node, cursor)
		if err != nil {
			return err
		}
		cursor = chunk.Next
		text.Write(chunk.Data)
		if a.Contains != nil && strings.Contains(text.String(), *a.Contains) {
			return nil
		}
		if a.EOF != nil && chunk.EOF == *a.EOF {
			return nil
		}
		if chunk.EOF {
			return errScenarioMismatch
		}
	}
}

func scenarioValidateAction(a Action) error {
	rules := map[ActionName]string{
		ActionSubmit: "ref root spec", ActionAdd: "ref parent items", ActionStop: "node",
		ActionInject: "node message_ref", ActionCapacityAvailable: "group", ActionRelease: "root",
		ActionClose: "?timeout_ms ?expect ?async", ActionAwaitStart: "node bind ?timeout_ms",
		ActionReleaseStart: "attempt decision ?state ?code ?timeout_ms", ActionComplete: "attempt state ?code ?timeout_ms",
		ActionEmit: "attempt text", ActionAwaitInject: "attempt message_ref ?timeout_ms",
		ActionHoldStop: "attempt", ActionFailStop: "attempt code", ActionAwaitStop: "attempt call ?timeout_ms",
		ActionReturnStop: "attempt call result", ActionHoldDispose: "attempt", ActionAwaitDispose: "attempt ?timeout_ms",
		ActionReturnDispose: "attempt result", ActionHoldRead: "attempt", ActionReturnRead: "attempt",
		ActionAwaitState: "node state ?timeout_ms", ActionExpectOutput: "node ?contains ?eof ?timeout_ms ?async",
		ActionJoin: "name ?timeout_ms", ActionAwaitRecord: "type ?node ?code ?timeout_ms",
	}
	rule, ok := rules[a.Action]
	if !ok {
		return scenarioInvalid("action")
	}
	allowed, required := map[string]bool{"action": true, "expect_error": true}, map[string]bool{}
	for _, f := range strings.Fields(rule) {
		name := strings.TrimPrefix(f, "?")
		allowed[name] = true
		if f == name {
			required[name] = true
		}
	}
	value, typ := reflect.ValueOf(a), reflect.TypeOf(a)
	for i := 1; i < value.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		present := !value.Field(i).IsNil()
		if present && !allowed[name] || !present && required[name] {
			return scenarioInvalid("action fields: " + name + " (rule: " + rule + ")")
		}
	}
	if a.Ref != nil && (*a.Ref == "" || !jobbatch.ValidRef(*a.Ref)) {
		return scenarioInvalid("ref")
	}
	if a.TimeoutMS != nil {
		if _, err := scenarioDuration(*a.TimeoutMS, true); err != nil {
			return err
		}
	}
	if a.ExpectError != nil && !scenarioErrorValid(*a.ExpectError) {
		return scenarioInvalid("expect_error")
	}
	if a.Expect != nil {
		if *a.Expect != CloseOK && *a.Expect != CloseTimeout && *a.Expect != CloseDispose {
			return scenarioInvalid("close expectation")
		}
		if a.ExpectError != nil {
			return scenarioInvalid("conflicting close expectations")
		}
	}
	for _, id := range []*jobbatch.ID{a.Root, a.Parent, a.Node} {
		if id != nil && *id == "" {
			return scenarioInvalid("node identity")
		}
	}
	if a.Action == ActionSubmit && strings.Contains(string(*a.Root), ".") {
		return scenarioInvalid("root identity")
	}
	if a.Call != nil && *a.Call == 0 {
		return scenarioInvalid("stop call")
	}
	if a.Result != nil && !controlled.ScenarioGateValid(*a.Result) {
		return scenarioInvalid("gate result")
	}
	if a.Action == ActionExpectOutput && (a.Contains != nil && a.EOF != nil || a.Contains == nil && a.EOF == nil && a.ExpectError == nil) {
		return scenarioInvalid("output expectation")
	}
	if a.Action == ActionReleaseStart {
		d := StartDecision{Decision: *a.Decision}
		if a.State != nil {
			d.State = *a.State
		}
		if a.Code != nil {
			d.Code = *a.Code
		}
		if !controlled.DecisionValid(d) || (d.Decision != DecisionAcceptComplete && a.State != nil) || ((d.Decision == DecisionAccept || d.Decision == DecisionRefuseCapacity) && a.Code != nil) {
			return scenarioInvalid("start decision")
		}
	}
	if a.State != nil {
		if a.Action == ActionAwaitState {
			if *a.State != "terminal" && !scenarioStateValid(*a.State) {
				return scenarioInvalid("state")
			}
		} else if !a.State.Reportable() {
			return scenarioInvalid("result state")
		}
	}
	if a.Type != nil && !scenarioRecordValid(*a.Type) {
		return scenarioInvalid("record type")
	}
	return nil
}

func scenarioErrorValid(e ErrorName) bool {
	switch e {
	case ErrorInvalidSpec, ErrorInvalidEvent, ErrorDuplicateRoot, ErrorUnknownNode, ErrorTerminal, ErrorNotTerminal, ErrorNotStarted, ErrorUnsupported, ErrorReleased, ErrorClosing, ErrorClosed, ErrorCloseTimeout, ErrorGap, ErrorExecutorPanic, ErrorExecutionDone, ErrorDispose, ErrorCapacity, ErrorUnknownAttempt, ErrorAttemptPhase:
		return true
	}
	return false
}

func scenarioStateValid(s jobbatch.State) bool {
	return s == jobbatch.StatePending || s.Active() || s.Terminal()
}

func scenarioRecordValid(t jobbatch.RecordType) bool {
	switch t {
	case jobbatch.RecordSubmit, jobbatch.RecordAdd, jobbatch.RecordStopRequested, jobbatch.RecordStarted, jobbatch.RecordStartRejected, jobbatch.RecordSettled, jobbatch.RecordControl, jobbatch.RecordCapacityAvailable, jobbatch.RecordDeadlineExpired, jobbatch.RecordHostClosing, jobbatch.RecordRelease, jobbatch.RecordCommand, jobbatch.RecordNotice, jobbatch.RecordClosed:
		return true
	}
	return false
}

func scenarioValidateExpect(e *Expect) error {
	if e == nil {
		return nil
	}
	if e.MaxActive != nil && *e.MaxActive < 0 || e.ExitCode != nil && (*e.ExitCode < 0 || *e.ExitCode > 5) {
		return scenarioInvalid("oracle number")
	}
	if e.Roots != nil {
		for _, r := range *e.Roots {
			if r.ID == "" || r.State != nil && !scenarioStateValid(*r.State) || !scenarioCountsValid(r.Counts) {
				return scenarioInvalid("root oracle")
			}
		}
	}
	if e.Nodes != nil {
		for _, n := range *e.Nodes {
			if n.ID == "" || n.State != nil && !scenarioStateValid(*n.State) || !scenarioCauseValid(n.Cause) {
				return scenarioInvalid("node oracle")
			}
			for _, p := range []*int{n.Attempts, n.Starts, n.Accepted, n.Stops, n.Disposes} {
				if p != nil && *p < 0 {
					return scenarioInvalid("oracle counter")
				}
			}
			if n.Outcome != nil {
				if n.Outcome.State != nil && *n.Outcome.State != "" && !n.Outcome.State.Reportable() || n.Outcome.Code != nil && !jobbatch.ValidCode(*n.Outcome.Code) {
					return scenarioInvalid("outcome oracle")
				}
			}
			if n.Control != nil {
				c := *n.Control
				if c != "none" && c != "pending" && c != "acked" && !(strings.HasPrefix(c, "failed(") && strings.HasSuffix(c, ")") && jobbatch.ValidCode(c[7:len(c)-1]) && c[7:len(c)-1] != "") {
					return scenarioInvalid("control oracle")
				}
			}
		}
	}
	if e.Notices != nil {
		for root, notices := range *e.Notices {
			if root == "" {
				return scenarioInvalid("notice root")
			}
			for _, n := range notices {
				if n.Node == "" || n.State != nil && !n.State.Terminal() || !scenarioCauseValid(n.Cause) || !scenarioCountsValid(n.Counts) {
					return scenarioInvalid("notice oracle")
				}
			}
		}
	}
	if e.Before != nil {
		for _, pair := range *e.Before {
			if pair[0] == "" || pair[1] == "" {
				return scenarioInvalid("before oracle")
			}
		}
	}
	return nil
}

func scenarioCauseValid(c *jobbatch.Cause) bool {
	return c == nil || *c == "" || *c == jobbatch.CauseStop || *c == jobbatch.CauseTimeout || *c == jobbatch.CauseHostClosing || *c == jobbatch.CauseStopOnFail
}

func scenarioCountsValid(c *jobbatch.Counts) bool {
	if c == nil {
		return true
	}
	b, _ := json.Marshal(c)
	var counts map[string]int
	_ = json.Unmarshal(b, &counts)
	for _, n := range counts {
		if n < 0 {
			return false
		}
	}
	return true
}
