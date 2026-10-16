package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestPhase2OracleNodeState(t *testing.T) {
	// P5/L6: a present but wrong node state is an assertion, not a wildcard.
	s, _, records, _ := labTestCapture(t)
	c := &journalCapture{scenario: s, records: records}
	nodes := []NodeExpectation{{ID: "A.1", State: labTestPtr(jobbatch.StateFailed)}}
	if c.checkOracle(&Expect{Nodes: &nodes}, NewOracle(), true) {
		t.Fatal("wrong node state accepted")
	}
}

func TestPhase2GapCleanupPrecedence(t *testing.T) {
	// F-c/C-d: actual cleanup failure outranks actual retained-feed loss.
	synctest.Test(t, func(t *testing.T) {
		s := labTestLoadFixture(t, "c-d")
		s.Runner.RecordRetention = 4
		w := &labTestJournal{}
		var h *RunHarness
		defer func() {
			if h != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := h.Runner.Close(ctx); err != nil && !errors.Is(err, jobbatch.ErrDispose) {
					t.Errorf("cleanup Close: %s", ErrorNameOf(err))
				}
			}
		}()
		got := RunScenario(context.Background(), s, RunOptions{
			OpenEvents:       func() (io.WriteCloser, error) { return w, nil },
			BeforeAction:     func(_ context.Context, _ int, run *RunHarness) error { h = run; return nil },
			BeforeEventsRead: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		})
		if got.Footer.Capture.Status != CaptureGap || got.Footer.Outcome.Status != OutcomeClosed {
			t.Fatalf("expected real closed gap: %+v", got)
		}
		if got.ExitCode != ExitUnfinished {
			t.Fatalf("gap+cleanup failure exit=%d want=3", got.ExitCode)
		}
	})
}

func TestPhase2FinalClosedComparison(t *testing.T) {
	// L6: compare the producer's Final tail directly, without metadata rejection.
	s, _, records, _ := labTestCapture(t)
	if records[len(records)-1].Type != jobbatch.RecordClosed {
		t.Fatal("baseline missing Final")
	}
	bad := append([]jobbatch.Record(nil), records...)
	bad[len(bad)-1].Code = "invented"
	if _, _, _, err := journalRebuild(s, bad, true); err == nil {
		t.Fatal("changed Final closed tail accepted")
	}
}

func TestPhase2SecondReleaseKeepsFirstDecision(t *testing.T) {
	// C-e: repeated release returns ErrAttemptPhase and preserves the first code.
	synctest.Test(t, func(t *testing.T) {
		c := NewControlledExecutor()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan jobbatch.Result, 1)
		start := make(chan error, 1)
		go func() {
			_, err := c.Start(ctx, jobbatch.Launch{ID: "R.1", Token: 1}, func(r jobbatch.Result) { done <- r })
			start <- err
		}()
		defer func() {
			cancel()
			select {
			case <-start:
			case <-time.After(time.Second):
				t.Error("Start cleanup join bound")
			}
		}()
		if _, err := c.AwaitStart(ctx, "R.1"); err != nil {
			t.Fatal(err)
		}
		if err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionAcceptComplete, State: jobbatch.StateCompleted, Code: "first"}); err != nil {
			t.Fatal(err)
		}
		err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionFail, Code: "second"})
		if !errors.Is(err, ErrAttemptPhase) {
			t.Errorf("secondRelease=%v want ErrAttemptPhase", err)
		}
		select {
		case r := <-done:
			if r.Code != "first" || r.State != jobbatch.StateCompleted {
				t.Errorf("first decision lost: %+v", r)
			}
		case <-ctx.Done():
			t.Error("report join bound")
		}
		c.mu.Lock()
		code := c.attempts[1].decision.Code
		c.mu.Unlock()
		if code != "first" {
			t.Errorf("first decision overwritten: %q", code)
		}
	})
}

func TestPhase2ClosedCleanupReplay(t *testing.T) {
	// §5.5/C-d: physical exit3 does not convert an intact closed replay to exit4.
	s := labTestLoadFixture(t, "c-d")
	w := &labTestJournal{}
	got := RunScenario(context.Background(), s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }})
	if got.ExitCode != ExitUnfinished || got.Footer.Capture.Status != CaptureComplete || got.Footer.Outcome.Status != OutcomeClosed {
		t.Fatalf("run=%+v", got)
	}
	if code := Replay(s, bytes.NewReader(w.Bytes())).ExitCode; code != ExitFailed {
		t.Fatalf("intact c-d replay=%d want=1", code)
	}
}
