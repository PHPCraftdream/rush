package lab

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestOracleClosedBeforeDisposeWithReaderHeld(t *testing.T) {
	// C-d mutation: using delayed journal-reader time for Closed falsely orders Dispose before Final.
	s := labTestLoadFixture(t, "c-d")
	w := &labTestJournal{}
	got := RunScenario(context.Background(), s, RunOptions{
		OpenEvents:       func() (io.WriteCloser, error) { return w, nil },
		BeforeEventsRead: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})
	if got.ExitCode != ExitUnfinished || got.Footer.Outcome.Status != OutcomeClosed || len(got.Mismatches) != 0 {
		t.Fatalf("result=%+v", got)
	}
}

func TestOracleExactZeroAndEmpty(t *testing.T) {
	// V-e mutation: treating pointer zero/empty as unchecked loses exact counter and notice assertions.
	s := labTestLoadFixture(t, "f-d")
	s.Expect = nil
	_, _, r, _ := labTestCapture(t)
	c := &journalCapture{scenario: s, records: r}
	o := NewOracle()
	zero := 0
	empty := map[jobbatch.ID][]NoticeExpectation{}
	if !c.checkOracle(nil, o, false) {
		t.Fatal("unchecked")
	}
	if c.checkOracle(&Expect{Notices: &empty}, o, false) {
		t.Fatal("empty notices ignored")
	}
	nodes := []NodeExpectation{{ID: "A.1", Starts: &zero}}
	if !c.checkOracle(&Expect{Nodes: &nodes, MaxActive: &zero}, o, false) {
		t.Fatal("exact zero failed")
	}
	o.attempts[1] = &oracleAttempt{node: "A.1", entry: 1, accepted: true}
	if c.checkOracle(&Expect{Nodes: &nodes}, o, false) {
		t.Fatal("zero starts ignored")
	}
	if c.checkOracle(&Expect{MaxActive: &zero}, o, false) {
		t.Fatal("zero overlap ignored")
	}
}

func TestOracleFinalProbeRequiresRealFinal(t *testing.T) {
	// C-d mutation: marking Closed at Dispose entry without checking producer Final invents publication.
	for _, final := range []uint64{0, 9} {
		o := NewOracle()
		o.attempts[1] = &oracleAttempt{node: "A.1"}
		o.finalProbe = func() jobbatch.EventPage {
			return jobbatch.EventPage{Head: 9, Final: final, Records: []jobbatch.Record{{Node: "unrelated", Type: jobbatch.RecordStarted}}}
		}
		e := &oracleExecution{Execution: labTestDisposer{}, o: o, token: 1}
		if err := (oracleDispose{e}).Dispose(context.Background()); err != nil {
			t.Fatal(err)
		}
		if o.labelTime("unrelated:started") != 0 {
			t.Fatal("Dispose probe replayed retained history inside control deadline")
		}
		closed, entry := o.labelTime("closed"), o.labelTime("#1:dispose_entered")
		if final == 0 && closed != 0 || final != 0 && (closed == 0 || closed >= entry) {
			t.Fatalf("Final=%d closed=%d entry=%d", final, closed, entry)
		}
	}
}

type labTestDisposer struct{}

func (labTestDisposer) Stop(context.Context) error    { return nil }
func (labTestDisposer) Dispose(context.Context) error { return nil }

func TestJournalPrivacySentinels(t *testing.T) {
	// E9 mutation: copying raw code, messages, output, Summary or Details into journal leaks task data.
	s := labTestLoadFixture(t, "f-d")
	const secret = "PRIVATE_SENTINEL/argv=credential"
	s.Expect = nil
	s.Script[4].Code = labTestPtr(secret)
	w := &labTestJournal{}
	got := RunScenario(context.Background(), s, RunOptions{
		OpenEvents: func() (io.WriteCloser, error) { return w, nil },
		BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
			if i != 4 {
				return nil
			}
			l := h.Controlled.order[0].launch
			if err := h.Runner.Inject(ctx, l.ID, []byte(`{"text":"`+secret+`"}`)); err != nil {
				return err
			}
			if err := h.Controlled.Emit(l.Token, []byte(secret)); err != nil {
				return err
			}
			return h.Controlled.Complete(ctx, l.Token, jobbatch.Result{State: jobbatch.StateCompleted, Code: secret, Summary: secret, Details: []byte(`{"secret":"` + secret + `"}`)})
		},
	})
	// The deliberately repeated scripted Complete mismatches, but must still drain safely.
	if got.ExitCode != ExitMismatch || got.Footer.Outcome.Status != OutcomeClosed {
		t.Fatalf("exit=%d", got.ExitCode)
	}
	if bytes.Contains(w.Bytes(), []byte(secret)) || bytes.Contains(w.Bytes(), []byte("credential")) {
		t.Fatal("journal leaked sentinel")
	}
}

func TestOracleSealedAlwaysDelegates(t *testing.T) {
	o := NewOracle()
	if err := o.finishPhysical(context.Background()); err != nil {
		t.Fatal(err)
	}
	delegate := &labTestForwarding{}
	ex := &oracleExecutor{o, map[string]jobbatch.Executor{"forward": delegate}}
	reports := 0
	e, err := ex.Start(context.Background(), jobbatch.Launch{Token: 1, Task: jobbatch.TaskSpec{Kind: "forward"}}, func(jobbatch.Result) { reports++ })
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.(jobbatch.OutputReader).ReadOutput(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := e.(jobbatch.Disposer).Dispose(context.Background()); err != nil {
		t.Fatal(err)
	}
	delegate.report(jobbatch.Result{State: jobbatch.StateCompleted})
	if delegate.starts != 1 || delegate.stops != 1 || delegate.reads != 1 || delegate.disposes != 1 || reports != 1 {
		t.Fatalf("delegate=%+v reports=%d", delegate, reports)
	}
	if o.clock != 0 || len(o.attempts) != 0 || len(o.labels) != 0 || o.physical != 0 {
		t.Fatal("sealed measurements changed")
	}
}

type labTestForwarding struct {
	starts, stops, reads, disposes int
	report                         func(jobbatch.Result)
}

func (e *labTestForwarding) Start(_ context.Context, _ jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	e.starts++
	e.report = report
	return e, nil
}

func (e *labTestForwarding) Stop(context.Context) error    { e.stops++; return nil }
func (e *labTestForwarding) Dispose(context.Context) error { e.disposes++; return nil }
func (e *labTestForwarding) ReadOutput(context.Context, uint64) (jobbatch.OutputChunk, error) {
	e.reads++
	return jobbatch.OutputChunk{}, nil
}

func labTestPtr[T any](v T) *T { return &v }
