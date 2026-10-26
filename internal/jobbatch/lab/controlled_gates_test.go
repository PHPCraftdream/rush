package lab

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestControlledPhysicalStopGates(t *testing.T) {
	// C-a/C-b mutation: releasing held Stop on context cancellation frees physical ownership early.
	ctx, c := labTestContext(t), NewControlledExecutor()
	observations := make(chan controlledObservation, 32)
	c.observe = func(o controlledObservation) { observations <- o }
	execution, reports := labTestAccepted(t, c, ctx, 1)
	if err := c.HoldStop(1); err != nil {
		t.Fatal(err)
	}
	if err := c.HoldStop(1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.FailNextStop(1, "ignored"); err != nil {
		t.Fatal(err)
	}
	if err := c.FailNextStop(1, "duplicate"); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.ReturnStop(1, 1, GateOK); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal("early return", err)
	}
	stopCtx, cancel := context.WithCancel(ctx)
	returned := make(chan error, 1)
	go func() { returned <- execution.Stop(stopCtx) }()
	if err := c.AwaitStop(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-returned:
		t.Fatal("context released physical gate")
	default:
	}
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCancelled}); err != nil {
		t.Fatal(err)
	}
	_ = labTestReceive(t, ctx, reports)
	if err := c.FailNextStop(1, "next"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReturnStop(1, 1, GateOK); err != nil {
		t.Fatal(err)
	}
	if err := labTestReceive(t, ctx, returned); err != nil {
		t.Fatal("held explicit result did not override armed failure", err)
	}
	if err := c.ReturnStop(1, 1, GateOK); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal("repeated return", err)
	}
	err := execution.Stop(ctx)
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "next" || err.Error() != "batchlab-error-sentinel" {
		t.Fatal("next physical arm not consumed")
	}
	if err := c.AwaitStop(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for len(observations) > 0 {
		o := <-observations
		if o.Kind == "stop_entered" || o.Kind == "stop_returned" || o.Kind == "dispose_entered" {
			kinds = append(kinds, o.Kind)
		}
	}
	want := []string{"stop_entered", "stop_returned", "stop_entered", "stop_returned", "dispose_entered"}
	if len(kinds) != len(want) {
		t.Fatalf("observations %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("observations %v", kinds)
		}
	}
}

func TestControlledDisposeGates(t *testing.T) {
	// C-d mutation: releasing held Dispose on timeout erases its actual physical entry.
	for _, result := range []GateResult{GateOK, "error:RAW/private", GatePanic} {
		t.Run(string(result), func(t *testing.T) {
			ctx, c := labTestContext(t), NewControlledExecutor()
			execution, reports := labTestAccepted(t, c, ctx, 1)
			if err := c.HoldDispose(1); err != nil {
				t.Fatal(err)
			}
			if err := c.HoldDispose(1); !errors.Is(err, ErrAttemptPhase) {
				t.Fatal(err)
			}
			if err := c.ReturnDispose(1, result); !errors.Is(err, ErrAttemptPhase) {
				t.Fatal(err)
			}
			if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCompleted}); err != nil {
				t.Fatal(err)
			}
			_ = labTestReceive(t, ctx, reports)
			done := make(chan error, 1)
			panicked := make(chan bool, 1)
			disposeCtx, cancel := context.WithCancel(ctx)
			go func() {
				defer func() {
					if recover() != nil {
						panicked <- true
					}
				}()
				done <- execution.(jobbatch.Disposer).Dispose(disposeCtx)
			}()
			if err := c.AwaitDispose(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := c.HoldDispose(1); !errors.Is(err, ErrAttemptPhase) {
				t.Fatal(err)
			}
			cancel()
			select {
			case <-done:
				t.Fatal("Dispose gate obeyed timeout")
			default:
			}
			if err := c.ReturnDispose(1, result); err != nil {
				t.Fatal(err)
			}
			if result == GatePanic {
				if !labTestReceive(t, ctx, panicked) {
					t.Fatal("missing panic")
				}
			} else {
				err := labTestReceive(t, ctx, done)
				if result == GateOK && err != nil {
					t.Fatal(err)
				}
				if result != GateOK {
					var coded interface{ Code() string }
					if !errors.As(err, &coded) || coded.Code() != "RAW/private" || err.Error() != "batchlab-error-sentinel" {
						t.Fatal("dispose error contract")
					}
				}
			}
			if err := c.AwaitDispose(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := c.ReturnDispose(1, result); !errors.Is(err, ErrUnknownAttempt) {
				t.Fatal(err)
			}
		})
	}
}

func TestControlledReadGateAndStream(t *testing.T) {
	// C-a-iii/R4 mutation: releasing a held read on cancellation loses bytes/EOF across report.
	ctx, c := labTestContext(t), NewControlledExecutor()
	observations := make(chan controlledObservation, 32)
	c.observe = func(o controlledObservation) { observations <- o }
	execution, reports := labTestAccepted(t, c, ctx, 1)
	reader := execution.(jobbatch.OutputReader)
	if err := c.ReturnRead(1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.HoldRead(1); err != nil {
		t.Fatal(err)
	}
	if err := c.HoldRead(1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithCancel(ctx)
	type readResult struct {
		chunk jobbatch.OutputChunk
		err   error
	}
	done := make(chan readResult, 1)
	go func() { chunk, err := reader.ReadOutput(readCtx, 0); done <- readResult{chunk, err} }()
	for {
		if o := labTestReceive(t, ctx, observations); o.Kind == "read_entered" {
			break
		}
	}
	cancel()
	select {
	case <-done:
		t.Fatal("read hold released on cancellation")
	default:
	}
	data := []byte("AB")
	if err := c.Emit(1, data); err != nil {
		t.Fatal(err)
	}
	data[0] = '!'
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCancelled}); err != nil {
		t.Fatal(err)
	}
	_ = labTestReceive(t, ctx, reports)
	if err := c.ReturnRead(1); err != nil {
		t.Fatal(err)
	}
	result := labTestReceive(t, ctx, done)
	if result.err != nil || string(result.chunk.Data) != "AB" || !result.chunk.EOF || result.chunk.Next != 2 {
		t.Fatal("held read stream result", result)
	}
	result.chunk.Data[0] = '!'
	chunk, err := reader.ReadOutput(ctx, 0)
	if err != nil || string(chunk.Data) != "AB" {
		t.Fatal("output alias", err)
	}
	if err := c.ReturnRead(1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestControlledOutputWaitsForData(t *testing.T) {
	// R4 mutation: returning empty output before data or EOF bypasses the read rendezvous.
	ctx, c := labTestContext(t), NewControlledExecutor()
	observations := make(chan controlledObservation, 32)
	c.observe = func(o controlledObservation) { observations <- o }
	execution, _ := labTestAccepted(t, c, ctx, 1)
	done := make(chan jobbatch.OutputChunk, 1)
	go func() { chunk, _ := execution.(jobbatch.OutputReader).ReadOutput(ctx, 0); done <- chunk }()
	for {
		if o := labTestReceive(t, ctx, observations); o.Kind == "read_entered" {
			break
		}
	}
	select {
	case <-done:
		t.Fatal("false EOF on empty live stream")
	default:
	}
	if err := c.Emit(1, []byte("A")); err != nil {
		t.Fatal(err)
	}
	chunk := labTestReceive(t, ctx, done)
	if string(chunk.Data) != "A" || chunk.EOF || chunk.Next != 1 {
		t.Fatal(chunk)
	}
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	chunk, err := execution.(jobbatch.OutputReader).ReadOutput(ctx, 1)
	if err != nil || !chunk.EOF || len(chunk.Data) != 0 {
		t.Fatal(chunk, err)
	}
	if err := execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}
