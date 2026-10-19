package jobbatch

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

// Item 2: the durable retirement path must clear the Launch payload at
// settlement, mirroring the non-durable path (runner.go:357), while keeping
// the attempt indexed for late physical returns and the frozen task-id miss
// (BatchView{}) in frozenSnapshots.
func TestRuntimeDurableRetiredPayloadCleared(t *testing.T) {
	t.Parallel()
	r, e, _, ctx := runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
	payload := []byte(`"` + strings.Repeat("x", 2048) + `"`)
	b := runtimeTestSubmit(t, ctx, r, "r", OneItem(TaskSpec{Kind: "test", Group: "g", Payload: payload}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	r.mu.Lock()
	if len(r.attempts) != 1 {
		t.Fatalf("attempts after durable settlement: %d", len(r.attempts))
	}
	var retired *runnerAttempt
	for _, at := range r.attempts {
		retired = at
	}
	kept := retired.launch.Task.Payload
	r.mu.Unlock()
	if kept != nil {
		t.Error("retained Launch payload after durable settlement")
	}
	frozen, hasFrozen := r.frozenSnapshots["r.1"]
	if !hasFrozen {
		t.Error("task id missing from the frozen-snapshot cache")
	} else if frozen.Sequence != 0 || frozen.Node != (NodeView{}) || frozen.Counts != (Counts{}) || frozen.Descendants != nil {
		t.Errorf("task id frozen snapshot not the cached BatchView{} miss: %+v", frozen)
	}
	if err := r.Release(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	remaining := len(r.attempts)
	r.mu.Unlock()
	if remaining != 0 {
		t.Errorf("root prune at publish left attempts: %d", remaining)
	}
	runtimeTestClose(t, ctx, r)
}

// Item 3: the loop-exit block must drop the Engine's own TaskSpec payloads.
// afterClosed (runner.go:490) serves post-close View/Snapshot from the frozen
// Runtime maps r.views/r.snapshots (never the Engine, never payloads), so
// observable identity is preserved by dropTaskPayloads.
func TestRuntimeFinalDropsEnginePayloads(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nil-hook", "durable"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			payload := []byte(`"` + strings.Repeat("x", 2048) + `"`)
			var r *Runner
			var e *runtimeTestExecutor
			var ctx context.Context
			if mode == "durable" {
				r, e, _, ctx = runtimeTestCommitHooks(t, func(context.Context, Transition) error { return nil }, nil)
			} else {
				r, e, _, ctx = runtimeTestNew(t, 1, 128, nil)
			}
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled", Payload: payload}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			runtimeTestWait(t, ctx, b)
			v0 := runtimeTestView(t, ctx, r, "R")
			s0, _ := r.Snapshot(ctx, "R")
			runtimeTestClose(t, ctx, r)
			// Structural: every Engine task node lost its payload; roots intact.
			eng := r.engine
			for id, n := range eng.nodes {
				if n.task != nil && len(n.task.Payload) != 0 {
					t.Errorf("node %s retained Engine payload after Final", id)
				}
			}
			if len(eng.roots) != 1 {
				t.Fatalf("roots after Final: %d", len(eng.roots))
			}
			// Observable identity: afterClosed serves frozen published state.
			v1, err := r.View(ctx, "R")
			if err != nil {
				t.Fatal(err)
			}
			if v1 != v0 {
				t.Error("View changed across Final payload drop")
			}
			s1, s1err := r.Snapshot(ctx, "R")
			if s1err != nil {
				t.Fatal(s1err)
			}
			// Close accepts HostClosing, one more accepted event, so the
			// Final snapshot carries Sequence+1 (Engine.Apply increments the
			// sequence per accepted event, engine.go:70; the Final capture
			// happens after that increment). Sequence advancement is the
			// documented observable; everything else must be identical.
			if s1.Sequence != s0.Sequence+1 {
				t.Errorf("post-close snapshot Sequence %d, want %d (HostClosing advance)", s1.Sequence, s0.Sequence+1)
			}
			s0Seq := s0
			s0Seq.Sequence = s1.Sequence
			if !reflect.DeepEqual(s0Seq, s1) {
				t.Error("Snapshot content changed across Final payload drop")
			}
		})
	}
}
