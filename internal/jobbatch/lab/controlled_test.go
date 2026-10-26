package lab

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type labTestStartResult struct {
	execution jobbatch.Execution
	err       error
}

func labTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func labTestStart(t *testing.T, c *ControlledExecutor, ctx context.Context, token uint64) (<-chan labTestStartResult, <-chan jobbatch.Result) {
	t.Helper()
	started, reports := make(chan labTestStartResult, 1), make(chan jobbatch.Result, 2)
	go func() {
		e, err := c.Start(ctx, jobbatch.Launch{ID: "R.1", Token: token, Task: jobbatch.TaskSpec{Kind: "controlled", Payload: json.RawMessage(`{"kind":"controlled"}`)}}, func(r jobbatch.Result) { reports <- r })
		started <- labTestStartResult{e, err}
	}()
	l, err := c.AwaitStart(ctx, "R.1")
	if err != nil || l.Token != token {
		t.Fatal("registration", err)
	}
	return started, reports
}

func labTestReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("barrier expired")
		var zero T
		return zero
	}
}

func labTestAccepted(t *testing.T, c *ControlledExecutor, ctx context.Context, token uint64) (jobbatch.Execution, <-chan jobbatch.Result) {
	t.Helper()
	started, reports := labTestStart(t, c, ctx, token)
	if err := c.ReleaseStart(ctx, token, StartDecision{Decision: DecisionAccept}); err != nil {
		t.Fatal(err)
	}
	out := labTestReceive(t, ctx, started)
	if out.err != nil || out.execution == nil {
		t.Fatal("acceptance", out.err)
	}
	return out.execution, reports
}

func TestControlledAttemptPhases(t *testing.T) {
	// C-e/F-b mutation: allowing early/repeated Complete queues duplicate reports.
	ctx, c := labTestContext(t), NewControlledExecutor()
	started, reports := labTestStart(t, c, ctx, 1)
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCompleted}); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.Emit(1, []byte("early")); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if _, err := c.AwaitInject(ctx, 1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.HoldDispose(1); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	if err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionAccept}); err != nil {
		t.Fatal(err)
	}
	out := labTestReceive(t, ctx, started)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionFail}); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal(err)
	}
	select {
	case <-reports:
		t.Fatal("early completion queued")
	default:
	}
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCompleted, Code: "RAW/unchanged"}); err != nil {
		t.Fatal(err)
	}
	if got := labTestReceive(t, ctx, reports); got.Code != "RAW/unchanged" {
		t.Fatal("code normalized in fixture")
	}
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateFailed}); !errors.Is(err, ErrUnknownAttempt) {
		t.Fatal(err)
	}
	if err := out.execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.AwaitDispose(ctx, 1); err != nil {
		t.Fatal("disposed entry not retained", err)
	}
	if err := c.HoldStop(1); !errors.Is(err, ErrUnknownAttempt) {
		t.Fatal(err)
	}
}

func TestControlledDecisionsAndCancellation(t *testing.T) {
	// C-e mutation: accepting invalid decisions or cancelling a decided attempt changes report ownership.
	for _, decision := range []StartDecision{
		{Decision: DecisionRefuseCapacity},
		{Decision: DecisionFail, Code: "PRIVATE_SENTINEL/argv=x"},
		{Decision: DecisionAcceptComplete, State: jobbatch.StateCompleted, Code: "inside"},
	} {
		t.Run(string(decision.Decision), func(t *testing.T) {
			ctx, c := labTestContext(t), NewControlledExecutor()
			started, reports := labTestStart(t, c, ctx, 1)
			if err := c.ReleaseStart(ctx, 1, decision); err != nil {
				t.Fatal(err)
			}
			out := labTestReceive(t, ctx, started)
			switch decision.Decision {
			case DecisionRefuseCapacity:
				if out.execution != nil || !errors.Is(out.err, jobbatch.ErrCapacity) {
					t.Fatal("capacity accepted")
				}
			case DecisionFail:
				var coded interface{ Code() string }
				if out.execution != nil || !errors.As(out.err, &coded) || coded.Code() != decision.Code || out.err.Error() != "batchlab-error-sentinel" {
					t.Fatal("raw failure contract")
				}
			case DecisionAcceptComplete:
				if out.execution == nil || out.err != nil || labTestReceive(t, ctx, reports).Code != "inside" {
					t.Fatal("inside Start completion")
				}
				if err := c.Complete(ctx, 1, jobbatch.Result{}); !errors.Is(err, ErrUnknownAttempt) {
					t.Fatal(err)
				}
				if err := out.execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if decision.Decision != DecisionAcceptComplete {
				if err := c.HoldStop(1); !errors.Is(err, ErrUnknownAttempt) {
					t.Fatal("retired token retained as live")
				}
				select {
				case <-reports:
					t.Fatal("rejected attempt reported")
				default:
				}
			}
		})
	}
	ctx, c := labTestContext(t), NewControlledExecutor()
	startCtx, cancel := context.WithCancel(ctx)
	started, reports := labTestStart(t, c, startCtx, 9)
	cancel()
	out := labTestReceive(t, ctx, started)
	if out.execution != nil || !errors.Is(out.err, context.Canceled) {
		t.Fatal("cancellation accepted")
	}
	if err := c.ReleaseStart(ctx, 9, StartDecision{Decision: DecisionAccept}); !errors.Is(err, ErrUnknownAttempt) {
		t.Fatal(err)
	}
	select {
	case <-reports:
		t.Fatal("cancelled start reported")
	default:
	}
}

func TestControlledCancelledAcknowledgementKeepsDecision(t *testing.T) {
	// C-e mutation: a cancelled acknowledgement must not roll back the selected decision.
	ctx, c := labTestContext(t), NewControlledExecutor()
	reportEntered, releaseReport := make(chan struct{}), make(chan struct{})
	started := make(chan labTestStartResult, 1)
	go func() {
		execution, err := c.Start(ctx, jobbatch.Launch{ID: "R.1", Token: 1}, func(jobbatch.Result) {
			close(reportEntered)
			<-releaseReport
		})
		started <- labTestStartResult{execution, err}
	}()
	if _, err := c.AwaitStart(ctx, "R.1"); err != nil {
		t.Fatal(err)
	}
	ackCtx, cancel := context.WithCancel(ctx)
	acknowledgement := make(chan error, 1)
	go func() {
		acknowledgement <- c.ReleaseStart(ackCtx, 1, StartDecision{Decision: DecisionAcceptComplete, State: jobbatch.StateCompleted})
	}()
	labTestReceive(t, ctx, reportEntered)
	cancel()
	if err := labTestReceive(t, ctx, acknowledgement); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionFail}); !errors.Is(err, ErrAttemptPhase) {
		t.Fatal("decision replaced", err)
	}
	close(releaseReport)
	out := labTestReceive(t, ctx, started)
	if out.err != nil || out.execution == nil {
		t.Fatal("first acceptance lost", out.err)
	}
	if err := out.execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestControlledAwaitOrderAndOwnership(t *testing.T) {
	// C-e mutation: reusing an awaited start or aliasing payloads corrupts attempt identity.
	ctx, c := labTestContext(t), NewControlledExecutor()
	execution, _ := labTestAccepted(t, c, ctx, 1)
	message := json.RawMessage(`{"go":true}`)
	if err := execution.(jobbatch.Messenger).Inject(ctx, message); err != nil {
		t.Fatal(err)
	}
	message[0] = '!'
	got, err := c.AwaitInject(ctx, 1)
	if err != nil || string(got) != `{"go":true}` {
		t.Fatal("message ownership", err)
	}
	started, _ := labTestStart(t, c, ctx, 2)
	if err := c.ReleaseStart(ctx, 2, StartDecision{Decision: DecisionFail}); err != nil {
		t.Fatal(err)
	}
	_ = labTestReceive(t, ctx, started)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.AwaitStart(cancelled, "R.1"); !errors.Is(err, context.Canceled) {
		t.Fatal("attempt awaited twice")
	}
	if err := c.Complete(ctx, 1, jobbatch.Result{State: jobbatch.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := execution.(jobbatch.Messenger).Inject(ctx, json.RawMessage(`null`)); !errors.Is(err, jobbatch.ErrExecutionDone) {
		t.Fatal(err)
	}
	if err := execution.(jobbatch.Disposer).Dispose(ctx); err != nil {
		t.Fatal(err)
	}
}
