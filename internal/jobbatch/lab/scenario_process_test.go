//go:build windows || linux || darwin || freebsd || openbsd || netbsd || dragonfly

package lab

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// Reuses the process owner's sync.Once build; no second helper build per binary.
func labTestHelper(t *testing.T) string { t.Helper(); return processTestHelper(t) }

func labTestProcessScenario(t *testing.T, modes ...[]string) *Scenario {
	t.Helper()
	s := &Scenario{Version: 1, Limits: jobbatch.Limits{MaxConcurrent: 3, MaxDepth: 8, MaxNodes: 32}, Runner: RunnerSettings{ControlTimeoutMS: 1000, RecordRetention: 256}, WaitBoundMS: 10000, Payloads: map[string]Payload{}}
	items := []Spec{}
	for i, args := range modes {
		name := fmt.Sprintf("p%d", i)
		s.Payloads[name] = Payload{Kind: "process", Program: labTestPtr(labTestHelper(t)), Args: labTestPtr(args)}
		items = append(items, Spec{Task: &TaskSpec{PayloadRef: name}})
	}
	s.Script = []Action{{Action: ActionSubmit, Root: labTestPtr(jobbatch.ID("R")), Ref: labTestPtr("process-run"), Spec: &Spec{Batch: &BatchSpec{Mode: jobbatch.ModeParallel, Items: items}}}}
	return s
}

func labTestAwait(id string, state jobbatch.State) Action {
	return Action{Action: ActionAwaitState, Node: labTestPtr(jobbatch.ID(id)), State: &state}
}
func labTestClose() Action { return Action{Action: ActionClose, Expect: labTestPtr(CloseOK)} }

func TestScenarioProcessL1(t *testing.T) {
	// Mutation: swallowing exit codes, treating rejected launches as accepted, or trusting the oracle must fail.
	for _, variant := range []string{"exit7", "missing", "wrong"} {
		t.Run(variant, func(t *testing.T) {
			s := labTestProcessScenario(t, []string{"exit", "0"}, []string{"exit", "7"}, []string{"exit", "0"})
			code, accepted, want := "exit_7", 1, ExitFailed
			if variant == "missing" {
				p := s.Payloads["p1"]
				p.Program = labTestPtr(filepath.Join(t.TempDir(), "missing-program"))
				s.Payloads["p1"] = p
				code = "missing_program"
				accepted = 0
			}
			if variant == "wrong" {
				code = "exit_8"
				want = ExitMismatch
			}
			s.Script = append(s.Script, labTestAwait("R", jobbatch.StateFailed), labTestClose())
			s.Expect = &Expect{Nodes: labTestPtr([]NodeExpectation{{ID: "R.2", Accepted: &accepted, Outcome: &OutcomeExpectation{State: labTestPtr(jobbatch.StateFailed), Code: &code}}})}
			got := RunScenario(context.Background(), s, RunOptions{})
			if got.ExitCode != want {
				t.Fatalf("exit=%d want=%d actions=%v oracle=%s", got.ExitCode, want, got.Mismatches, got.OracleRule)
			}
		})
	}
}

func labTestSentinel(t *testing.T) int {
	t.Helper()
	cmd := platform.Command(context.Background(), labTestHelper(t), "hold")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = pipe.Close()
		processTestWaitPID(t, pid)
		if processTestAlive(pid) {
			t.Errorf("leakedPID sentinel %d", pid)
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pipe).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != fmt.Sprintf("ready %d\n", pid) {
			t.Fatalf("sentinel handshake %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sentinel handshake bound")
	}
	return pid
}

func labTestPublicPIDs(t *testing.T, ctx context.Context, h *RunHarness, id jobbatch.ID, spawn bool) []int {
	t.Helper()
	ok := false
	defer func() {
		if !ok {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.Runner.Close(cleanup); err != nil {
				t.Errorf("PID handshake cleanup Close: %v", err)
			}
		}
	}()
	needle := "ready "
	if spawn {
		needle = "descendant-ready "
	}
	var text string
	var cursor uint64
	for !strings.Contains(text, needle) || !strings.HasSuffix(text, "\n") || spawn && !strings.Contains(text, "\nready ") {
		chunk, err := h.Runner.Output(ctx, id, cursor)
		if err != nil {
			t.Fatal(err)
		}
		text += string(chunk.Data)
		cursor = chunk.Next
		if chunk.EOF {
			t.Fatalf("premature EOF %q", text)
		}
	}
	var pids []int
	for _, line := range strings.Split(text, "\n") {
		var pid int
		if _, err := fmt.Sscanf(line, "ready %d", &pid); err == nil {
			pids = append(pids, pid)
		}
		if _, err := fmt.Sscanf(line, "descendant-ready %d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		t.Fatalf("missing PIDs %q", text)
	}
	for _, pid := range pids {
		pid := pid
		if !processTestAlive(pid) {
			t.Fatalf("not alive at handshake %d", pid)
		}
		t.Cleanup(func() {
			if processTestAlive(pid) {
				processTestKillPID(pid)
				processTestWaitPID(t, pid)
				t.Errorf("leakedPID %d", pid)
			}
		})
	}
	t.Logf("public output %s: %q PIDs=%v", id, text, pids)
	ok = true
	return pids
}

func labTestDead(t *testing.T, pids []int) {
	t.Helper()
	for _, pid := range pids {
		if processTestAlive(pid) {
			t.Errorf("tree alive at settlement/Close %d", pid)
		}
	}
}

func TestScenarioProcessL2(t *testing.T) {
	// Mutation: leader-only termination, premature settlement or collateral kills loses public tree isolation.
	sentinel := labTestSentinel(t)
	s := labTestProcessScenario(t, []string{"spawn"}, []string{"hold"})
	s.Script = append(s.Script, labTestAwait("R.1", jobbatch.StateRunning), labTestAwait("R.2", jobbatch.StateRunning), Action{Action: ActionStop, Node: labTestPtr(jobbatch.ID("R.1"))}, labTestAwait("R.1", jobbatch.StateCancelled), Action{Action: ActionStop, Node: labTestPtr(jobbatch.ID("R.2"))}, labTestAwait("R", jobbatch.StateCancelled), labTestClose())
	var tree, sibling []int
	got := RunScenario(context.Background(), s, RunOptions{BeforeAction: func(ctx context.Context, i int, h *RunHarness) error {
		if i == 3 {
			tree = labTestPublicPIDs(t, ctx, h, "R.1", true)
			sibling = labTestPublicPIDs(t, ctx, h, "R.2", false)
		}
		if i == 5 {
			labTestDead(t, tree)
			for _, pid := range append(sibling, sentinel) {
				if !processTestAlive(pid) {
					t.Errorf("collateral kill %d", pid)
				}
			}
			chunk, err := h.Runner.Output(ctx, "R.2", 0)
			if err != nil || chunk.EOF || !strings.Contains(string(chunk.Data), "ready ") {
				t.Errorf("sibling output %+v %v", chunk, err)
			}
		}
		return nil
	}})
	if got.ExitCode != ExitFailed {
		t.Fatalf("exit=%d actions=%v oracle=%s", got.ExitCode, got.Mismatches, got.OracleRule)
	}
	labTestDead(t, tree)
	labTestDead(t, sibling)
}

func TestScenarioProcessL5(t *testing.T) {
	// Mutation: using cancelled invocation context for Close leaks own trees or kills unrelated work.
	for _, mode := range []string{"invocation", "waitbound"} {
		t.Run(mode, func(t *testing.T) {
			sentinel := labTestSentinel(t)
			s := labTestProcessScenario(t, []string{"spawn"})
			s.Script = append(s.Script, labTestAwait("R.1", jobbatch.StateRunning), labTestClose())
			if mode == "waitbound" {
				s.WaitBoundMS = 2000
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var tree []int
			got := RunScenario(ctx, s, RunOptions{CleanupBound: 5 * time.Second, BeforeAction: func(bound context.Context, i int, h *RunHarness) error {
				if i == 2 {
					tree = labTestPublicPIDs(t, bound, h, "R.1", true)
					if mode == "invocation" {
						cancel()
					} else {
						<-bound.Done()
					}
				}
				return nil
			}})
			if got.ExitCode != ExitUnfinished {
				t.Fatalf("exit=%d outcome=%s", got.ExitCode, got.Footer.Outcome.Status)
			}
			labTestDead(t, tree)
			if !processTestAlive(sentinel) {
				t.Fatal("unrelated sentinel killed")
			}
			t.Logf("exit3 own trees dead %v unrelated alive %d", tree, sentinel)
		})
	}
}
