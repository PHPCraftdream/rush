package lab

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestScenarioExternalAsyncHookJoinIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := labTestLoadFixture(t, "f-d")

		s.Expect = nil

		s.Script = s.Script[:len(s.Script)-1]

		name := "close"

		s.Script = append(s.Script, Action{Action: ActionClose, Async: &name}, Action{Action: ActionJoin, Name: &name, TimeoutMS: labTestPtr(int64(1))})

		release := make(chan struct{})

		done := make(chan struct{})

		var h *RunHarness

		defer func() { labTestJoinHarness(t, h) }()

		defer func() { close(release); <-done }()

		got := RunScenario(context.Background(), s, RunOptions{BeforeAction: func(_ context.Context, _ int, run *RunHarness) error { h = run; return nil }, CleanupBound: time.Millisecond, BeforeCloseReturn: func(context.Context) error {
			defer close(done)

			<-release // Deliberately ignore cancellation to exercise the limitation.

			return nil
		}})

		if got.ExitCode != ExitUnfinished {
			t.Fatalf("result=%+v", got)
		}
	})
}

func TestCleanupReadsLeavesScriptedControlsHeld(t *testing.T) {
	c := NewControlledExecutor()

	stop := &controlledGate{done: make(chan struct{})}

	dispose := &controlledGate{done: make(chan struct{})}

	read := &controlledGate{done: make(chan struct{})}

	a := &controlledAttempt{holdStop: true, holdDispose: true, stops: []*controlledGate{stop}, dispose: dispose, read: read}

	c.order = append(c.order, a)

	c.releaseCleanupReads()

	if !read.released || stop.released || dispose.released || !a.holdStop || !a.holdDispose {
		t.Fatal("read teardown changed scripted controls")
	}

	for _, g := range []*controlledGate{stop, dispose} {
		select {
		case <-g.done:

			t.Fatal("scripted control returned during teardown")

		default:
		}
	}
}

func TestScenarioNormalDisposeTimeoutLeavesPhysicalGateHeld(t *testing.T) {
	s := labTestLoadFixture(t, "c-d")

	s.Expect = nil

	// Replace the scripted dispose return with its logical timeout and join.

	s.Script = append(s.Script[:14], s.Script[15:]...)

	s.Script[11].TimeoutMS = labTestPtr(int64(1))

	var h *RunHarness

	got := RunScenario(context.Background(), s, RunOptions{BeforeAction: func(_ context.Context, _ int, run *RunHarness) error { h = run; return nil }})

	if got.ExitCode != ExitUnfinished {
		t.Fatalf("result=%+v", got)
	}

	h.Controlled.mu.Lock()

	g := h.Controlled.order[0].dispose

	held := g != nil && !g.released

	h.Controlled.mu.Unlock()

	if !held {
		t.Fatal("normal capture released physical dispose")
	}

	// Real cleanup remains possible after Oracle measurement sealing.

	if err := h.Controlled.ReturnDispose(h.Controlled.order[0].launch.Token, GateOK); err != nil {
		t.Fatal(err)
	}
}

func TestScenarioReadEntryBarrierAndCleanupJoin(t *testing.T) {
	// C-a-iii mutation: returning from a cancelled async Output before its physical held read returns races disposal/oracle.

	s := labTestLoadFixture(t, "f-d")

	s.Expect = nil

	held, async, contains := "a1", "read", "never-emitted"

	s.Script = append(s.Script[:4], []Action{
		{Action: ActionHoldRead, Attempt: &held},

		{Action: ActionExpectOutput, Node: labTestPtr(jobbatch.ID("A.1")), Contains: &contains, Async: &async},

		{Action: ActionComplete, Attempt: &held, State: labTestPtr(jobbatch.StateCompleted)},

		{Action: ActionAwaitState, Node: labTestPtr(jobbatch.ID("A")), State: labTestPtr(jobbatch.StateCompleted)},

		{Action: ActionJoin, Name: &async},

		{Action: ActionClose},
	}...)

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	w := &labTestJournal{}

	var oracle *Oracle

	got := RunScenario(ctx, s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }, BeforeAction: func(actionCtx context.Context, i int, h *RunHarness) error {
		oracle = h.Oracle

		if i == 6 {
			if err := h.Oracle.AwaitLabel(actionCtx, "a1:read_entered"); err != nil {
				return err
			}

			cancel()
		}

		return nil
	}})

	if got.ExitCode != ExitUnfinished {
		t.Fatalf("exit=%d", got.ExitCode)
	}

	oracle.mu.Lock()

	defer oracle.mu.Unlock()

	if oracle.physical != 0 || !oracle.sealed || oracle.labelTime("a1:read_returned") == 0 {
		t.Fatal("physical read outlived run")
	}
}
