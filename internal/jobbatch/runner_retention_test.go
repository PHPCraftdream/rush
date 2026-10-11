package jobbatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// §§2.2/3.4/4.5: give-up drops Runtime ownership, not pinned calls or
// frozen final inspection; Close retains cleanup metadata, never Execution.
func TestRuntimeGiveUpDropsReferences(t *testing.T) {
	for _, mode := range []string{"release", "final", "no-capability", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var r *Runner
			inspected := make(chan [4]int, 1)
			hooks := &runnerRequestHooks{reply: func(q *runnerRequest) {
				if q.event.Type != EventCapacityAvailable {
					return
				}
				stats := [4]int{len(r.attempts), len(r.batches), len(r.views), len(r.snapshots)}
				inspected <- stats
			}}
			var e *runtimeTestExecutor
			var c *runtimeTestClock
			var ctx context.Context
			r, e, c, ctx = runtimeTestNew(t, 1, 128, hooks)
			entered, unblock := make(chan struct{}, 1), make(chan struct{})
			var x Execution = &runtimeTestExecution{dispose: func(context.Context) error { entered <- struct{}{}; <-unblock; return nil }}
			if mode == "no-capability" {
				x = runtimeTestRetentionBareExecution{}
			}
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled", Payload: []byte(`"payload"`)}))
			a := runtimeTestAccept(t, ctx, r, e, x)
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			summary := runtimeTestWait(t, ctx, b)
			r.mu.Lock()
			h := r.handles[0]
			r.mu.Unlock()
			if mode == "final" {
				done := make(chan error, 1)
				go func() { done <- r.Close(ctx) }()
				runtimeTestReceive(t, ctx, entered)
				close(unblock)
				if err := runtimeTestReceive(t, ctx, done); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.Release(ctx, "R"); err != nil {
					t.Fatal(err)
				}
				if mode != "no-capability" {
					runtimeTestReceive(t, ctx, entered)
					runtimeTestReceive(t, ctx, c.armed)
					if mode == "timeout" {
						c.advance(50 * time.Second)
					} else {
						close(unblock)
					}
					// Logical cleanup is a deterministic barrier, independently of return.
					for {
						_, pending, changed := r.cleanupStatus()
						if !pending {
							break
						}
						runtimeTestReceive(t, ctx, changed)
					}
				}
				if err := r.CapacityAvailable(ctx, ""); err != nil {
					t.Fatal(err)
				}
				stats := runtimeTestReceive(t, ctx, inspected)
				if stats != [4]int{} {
					t.Errorf("retained runtime maps=%v", stats)
				}
			}
			h.mu.Lock()
			retained := h.execution != nil
			h.mu.Unlock()
			if retained {
				t.Error("retained Execution after logical cleanup")
			}
			if mode == "timeout" {
				close(unblock)
			}
			if got := runtimeTestWait(t, ctx, b); got != summary {
				t.Fatal(got)
			}
			err := r.Close(ctx)
			if mode == "timeout" {
				if !errors.Is(err, ErrDispose) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "final" {
				if _, err := r.Snapshot(ctx, "R"); err != nil {
					t.Fatal(err)
				}
				if len(r.attempts) != 0 || len(r.batches) != 0 {
					t.Error("final retained attempts/batches")
				}
			} else if _, err := r.View(ctx, "R.1"); !errors.Is(err, ErrReleased) {
				t.Fatal(err)
			}
		})
	}
}

// C-a/C-b/§4.5: Release cannot clear Execution across held read/Stop pins;
// timeout is not actual return. Clearing early or retaining after cleanup fails.
func TestRuntimeRetentionHeldCalls(t *testing.T) {
	for _, mode := range []string{"read", "stop"} {
		t.Run(mode, func(t *testing.T) {
			returned := make(chan struct{}, 8)
			r, e, c, ctx := runtimeTestNew(t, 1, 128, &runnerRequestHooks{controlReturned: func() { returned <- struct{}{} }})
			entered, unblock := make(chan struct{}, 1), make(chan struct{})
			disposed := make(chan struct{}, 1)
			x := &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }}
			x.read = func(context.Context, uint64) (OutputChunk, error) {
				entered <- struct{}{}
				<-unblock
				return OutputChunk{}, nil
			}
			x.stop = func(context.Context) error { entered <- struct{}{}; <-unblock; return nil }
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
			a := runtimeTestAccept(t, ctx, r, e, x)
			readReturned := make(chan error, 1)
			if mode == "read" {
				go func() { _, err := r.Output(ctx, a.launch.ID, 0); readReturned <- err }()
			} else {
				if err := r.Stop(ctx, a.launch.ID); err != nil {
					t.Fatal(err)
				}
			}
			runtimeTestReceive(t, ctx, entered)
			if mode == "stop" {
				runtimeTestReceive(t, ctx, c.armed)
				c.advance(50 * time.Second)
				runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
			}
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			runtimeTestWait(t, ctx, b)
			r.mu.Lock()
			h := r.handles[0]
			r.mu.Unlock()
			if err := r.Release(ctx, "R"); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			retained, pins := h.execution == x, h.pins
			h.mu.Unlock()
			if !retained || pins != 1 {
				t.Errorf("Execution lost before actual %s return: retained=%v pins=%d", mode, retained, pins)
			}
			select {
			case <-disposed:
				t.Error("Dispose crossed held call")
			default:
			}
			close(unblock)
			if mode == "read" {
				if err := runtimeTestReceive(t, ctx, readReturned); err != nil {
					t.Fatal(err)
				}
			}
			runtimeTestReceive(t, ctx, disposed)
			runtimeTestClose(t, ctx, r)
			h.mu.Lock()
			retained = h.execution != nil
			pins = h.pins
			h.mu.Unlock()
			if retained || pins != 0 {
				t.Errorf("Execution retained after actual return and cleanup: retained=%v pins=%d", retained, pins)
			}
		})
	}
}

type runtimeTestRetentionBareExecution struct{}

func (runtimeTestRetentionBareExecution) Stop(context.Context) error { return nil }
