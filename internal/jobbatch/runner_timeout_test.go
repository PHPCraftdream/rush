package jobbatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// §§3.1.2/4.5: timeout must claim the one-shot result before cancellation
// wakes an executor returning ctx.Err. Force that schedule without sleeps.
func TestRuntimeTimeoutClaimsBeforeCancel(t *testing.T) {
	for _, op := range []string{"stop", "dispose"} {
		t.Run(op, func(t *testing.T) {
			wrong := 0
			for i := 0; i < 200; i++ {
				returned := make(chan struct{}, 1)
				hooks := &runnerRequestHooks{
					timeoutCancel:   func(cancel context.CancelFunc) { cancel(); <-returned },
					controlReturned: func() { returned <- struct{}{} },
				}
				r, e, c, ctx := runtimeTestNew(t, 1, 128, hooks)
				entered := make(chan struct{}, 1)
				call := func(c context.Context) error { entered <- struct{}{}; <-c.Done(); return c.Err() }
				x := &runtimeTestExecution{}
				if op == "stop" {
					x.stop = call
				} else {
					x.dispose = call
				}
				b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
				a := runtimeTestAccept(t, ctx, r, e, x)
				if op == "stop" {
					if err := r.Stop(ctx, a.launch.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					runtimeTestFinish(t, ctx, r, a, StateCompleted)
					runtimeTestWait(t, ctx, b)
					if err := r.Release(ctx, "R"); err != nil {
						t.Fatal(err)
					}
				}
				runtimeTestReceive(t, ctx, entered)
				runtimeTestReceive(t, ctx, c.armed)
				c.advance(50 * time.Second)
				if op == "stop" {
					rec := runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
					if rec.Code != CodeStopTimeout {
						wrong++
					}
					runtimeTestFinish(t, ctx, r, a, StateCompleted)
					runtimeTestWait(t, ctx, b)
					runtimeTestClose(t, ctx, r)
				} else {
					err := r.Close(ctx)
					var d *DisposeError
					if !errors.As(err, &d) || len(d.Failures) != 1 {
						t.Fatal(err)
					}
					if d.Failures[0].Code != CodeDisposeTimeout {
						wrong++
					}
					if again := r.Close(ctx); again.Error() != err.Error() {
						t.Fatal(again)
					}
				}
			}
			if wrong != 0 {
				t.Fatalf("%s: cancellation won %d/200 timeout latches", op, wrong)
			}
		})
	}
}
