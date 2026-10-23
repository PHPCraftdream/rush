package lab

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func TestScenarioOutputErrorOnly(t *testing.T) {
	// Mutation: requiring a data predicate for a typed capability error weakens frozen R-a acceptance.
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"action":"expect_output","node":"B.1","expect_error":"not_started"}`, true},
		{`{"action":"expect_output","node":"B.1"}`, false},
		{`{"action":"expect_output","node":"B.1","contains":"","eof":true,"expect_error":"not_started"}`, false},
		{`{"action":"expect_output","node":"B.1","eof":true,"expect_error":"not_started"}`, true},
	} {
		var a Action
		if err := json.Unmarshal([]byte(tc.raw), &a); err != nil {
			t.Fatal(err)
		}
		if (scenarioValidateAction(a) == nil) != tc.valid {
			t.Fatalf("validation %s", tc.raw)
		}
	}
	for _, name := range []string{"r-a", "r-a-h-held", "r-a-h-blocked", "f-b"} {
		s := labTestLoadFixture(t, name)
		if name == "f-b" {
			for i := range s.Script {
				if s.Script[i].Action == ActionExpectOutput {
					s.Script[i].EOF = nil
					s.Script[i].Contains = nil
				}
			}
		}
		got := RunScenario(context.Background(), s, RunOptions{})
		if got.ExitCode != *s.Expect.ExitCode {
			t.Fatalf("%s exit=%d actions=%v oracle=%s", name, got.ExitCode, got.Mismatches, got.OracleRule)
		}
	}
}

func TestScenarioWCLifecycle(t *testing.T) {
	// Mutation: erasing live timeout/failure gates or replacing a root's own HostClosing cause contradicts lifecycle 3.1/3.1.2.
	for _, name := range []string{"w-c-i", "w-c-ii"} {
		t.Run(name, func(t *testing.T) {
			s := labTestLoadFixture(t, name)
			got := labTestRunScenario(t, name, s, RunOptions{BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
				a := s.Script[i]
				if a.Action == ActionFailStop || a.Action == ActionComplete && a.Attempt != nil && *a.Attempt == "x1" && name == "w-c-ii" {
					if err := h.Oracle.AwaitLabel(ctx, "x1:stop_returned#1"); err != nil {
						return err
					}
					v, err := h.Runner.View(ctx, "X.1")
					if err != nil {
						return err
					}
					if v.Control != jobbatch.ControlFailed || v.ControlError != "stop_timeout" || v.ControlPending {
						t.Fatalf("actual return must retain failed gate: %+v", v)
					}
				}
				if a.Action == ActionSubmit && a.Root != nil && *a.Root == "A" {
					v, err := h.Runner.View(ctx, "X.1")
					if err != nil {
						t.Fatal(err)
					}
					if v.Control != jobbatch.ControlFailed || v.ControlError != "stop_timeout" {
						t.Fatalf("live timeout gate %+v", v)
					}
					t.Logf("live timed gate: %+v", v)
				}
				if a.Action == ActionComplete && a.Attempt != nil && *a.Attempt == "x1" && name == "w-c-i" {
					v, err := h.Runner.View(ctx, "X.1")
					if err != nil {
						t.Fatal(err)
					}
					if v.Control != jobbatch.ControlFailed || v.ControlError != "stop_failed" {
						t.Fatalf("live failed gate %+v", v)
					}
				}
				if a.Action == ActionClose && i == len(s.Script)-1 {
					v, err := h.Runner.Snapshot(ctx, "X")
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("final public Snapshot X: %+v", v)
					if name == "w-c-ii" && (v.Node.State != jobbatch.StateInterrupted || v.Node.Cause != jobbatch.CauseHostClosing || v.Counts.Cancelled != 1) {
						t.Fatalf("root own cause must win over leaf reducer: %+v", v)
					}
					if name == "w-c-i" && (len(v.Descendants) != 1 || v.Descendants[0].Control != jobbatch.ControlNone || v.Descendants[0].ControlPending) {
						t.Fatalf("settlement must retire gate: %+v", v)
					}
					p, err := h.Runner.Events(ctx, 1)
					if err != nil {
						t.Fatal(err)
					}
					for _, r := range p.Records {
						if r.Node == "X" || r.Node == "X.1" || r.Type == jobbatch.RecordHostClosing {
							t.Logf("runner record: %+v", r)
						}
					}
				}
				return nil
			}})
			if got.ExitCode != *s.Expect.ExitCode || len(got.Mismatches) != 0 || got.OracleRule != "" {
				t.Fatalf("exit=%d actions=%v oracle=%s", got.ExitCode, got.Mismatches, got.OracleRule)
			}
		})
	}
}

func TestScenarioL5WithheldConfirmation(t *testing.T) {
	// Mutation: treating Close timeout as termination releases held admission and falsely succeeds.
	s := labTestLoadFixture(t, "f-e")
	var owned *RunHarness
	defer func() { labTestJoinHarness(t, owned) }()
	got := RunScenario(context.Background(), s, RunOptions{BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
		owned = h
		if s.Script[i].Action == ActionComplete {
			v, err := h.Runner.View(ctx, "A.1")
			if err != nil {
				return err
			}
			if v.State != jobbatch.StateCancelling || !v.State.Active() {
				t.Fatalf("confirmation withheld lost reservation: %+v", v)
			}
			snapshot, err := h.Runner.Snapshot(ctx, "A")
			if err != nil {
				return err
			}
			if snapshot.Counts.Active != 1 {
				t.Fatalf("held admission %+v", snapshot)
			}
		}
		return nil
	}})
	if got.ExitCode != ExitUnfinished {
		t.Fatalf("exit=%d", got.ExitCode)
	}
}

func TestScenarioRejectedReturnStopHasNoPhysicalFence(t *testing.T) {
	// Mutation: fencing the action after a REJECTED return_stop on a physical return that cannot exist
	// turns a valid expect_error script into an unfinished run (§5.2: a matched error has no deferred effect).
	s := labTestLoadFixture(t, "c-e")
	rejected := Action{
		Action:      ActionReturnStop,
		Attempt:     labTestPtr("a1"),
		Call:        labTestPtr(uint64(1)),
		Result:      labTestPtr(GateOK),
		ExpectError: labTestPtr(ErrorAttemptPhase),
	}
	script := append([]Action(nil), s.Script[:2]...)
	script = append(script, rejected)
	s.Script = append(script, s.Script[2:]...)
	got := labTestRunScenario(t, "c-e-rejected-return-stop", s, RunOptions{})
	if got.ExitCode != *s.Expect.ExitCode || len(got.Mismatches) != 0 {
		t.Fatalf("exit=%d want=%d mismatches=%v oracle=%s", got.ExitCode, *s.Expect.ExitCode, got.Mismatches, got.OracleRule)
	}
}
