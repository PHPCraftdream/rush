package lab

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

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
		return run.c.ReleaseStart(ctx, token, StartDecision{Decision: *a.Decision, State: scenarioValue(a.State), Code: scenarioValue(a.Code)})
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
