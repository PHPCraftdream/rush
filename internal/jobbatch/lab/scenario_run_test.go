package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func labTestLoadFixture(t *testing.T, name string) *Scenario {
	t.Helper()
	data, err := os.ReadFile("testdata/scenarios/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type labTestJournal struct{ bytes.Buffer }

func (w *labTestJournal) Close() error { return nil }
func TestScenarioInterpreterRetainsUndeclaredMismatch(t *testing.T) {
	// C-e mutation: removing the first expected phase error must not skip cleanup.
	s := labTestLoadFixture(t, "c-e-undeclared")
	result := RunScenario(context.Background(), s, RunOptions{})
	if result.ExitCode != ExitMismatch || result.Footer.Outcome.Status != OutcomeClosed {
		t.Fatalf("exit=%d outcome=%s", result.ExitCode, result.Footer.Outcome.Status)
	}
}

func TestScenarioInterpreterInsideStart(t *testing.T) {
	// V-e mutation: dropping acceptance when report precedes Start return loses the physical execution.
	s := labTestLoadFixture(t, "v-e-task")
	result := RunScenario(context.Background(), s, RunOptions{})
	if result.ExitCode != ExitSuccess {
		t.Fatalf("exit=%d", result.ExitCode)
	}
}

func TestJournalReplayAndMutations(t *testing.T) {
	// F-d/L6 mutation: accepting removed Final/footer or uncounted capacity records corrupts replay.
	s := labTestLoadFixture(t, "f-d")
	w := &labTestJournal{}
	result := RunScenario(context.Background(), s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }})
	if result.ExitCode != ExitSuccess {
		t.Fatalf("run exit=%d", result.ExitCode)
	}
	if got := Replay(s, bytes.NewReader(w.Bytes())).ExitCode; got != ExitSuccess {
		t.Fatalf("replay exit=%d", got)
	}
	lines := bytes.Split(bytes.TrimSpace(w.Bytes()), []byte("\n"))
	mutations := [][]byte{}
	// L6 mutation: footer removal invalidates framing.
	mutations = append(mutations, bytes.Join(lines[:len(lines)-1], []byte("\n")))
	// F-d mutation: removing closed retains an authoritative footer contradiction.
	copyLines := append([][]byte(nil), lines[:len(lines)-2]...)
	copyLines = append(copyLines, lines[len(lines)-1])
	mutations = append(mutations, bytes.Join(copyLines, []byte("\n")))
	// V-d mutation: a nil capacity reply requires an accepted record.
	var footer JournalFooter
	if err := json.Unmarshal(lines[len(lines)-1], &footer); err != nil {
		t.Fatal(err)
	}
	footer.RequestCounts.CapacityAvailable++
	bad, _ := json.Marshal(footer)
	copyLines = append([][]byte(nil), lines...)
	copyLines[len(copyLines)-1] = bad
	mutations = append(mutations, bytes.Join(copyLines, []byte("\n")))
	for i, data := range mutations {
		if got := Replay(s, bytes.NewReader(data)).ExitCode; got != ExitInvalid {
			t.Fatalf("mutation %d exit=%d", i, got)
		}
	}
}

func TestScenarioExistingAcceptanceScripts(t *testing.T) {
	// R/C/W/V/R3 mutation: bypassing real Runner ingress or ignoring declared exits loses fixture coverage.
	paths, err := filepath.Glob("testdata/scenarios/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var declared Scenario
			if err := json.Unmarshal(data, &declared); err != nil {
				t.Fatal(err)
			}
			invalid := strings.HasPrefix(name, "v-a-")
			if invalid {
				if _, err := LoadScenario(strings.NewReader(string(data))); err == nil {
					t.Fatal("invalid fixture accepted")
				}
				return
			}
			s := labTestLoadFixture(t, name)
			got := labTestRunScenario(t, name, s, RunOptions{})
			if s.Expect == nil || s.Expect.ExitCode == nil || got.ExitCode != *s.Expect.ExitCode {
				t.Fatalf("exit=%d actions=%v oracle=%s close=%+v", got.ExitCode, got.Mismatches, got.OracleRule, got.CloseFailures)
			}
		})
	}
}

func TestOracleInsideStartIntervals(t *testing.T) {
	// V-e mutation: counting only reports after Start return loses synchronous acceptance overlap.
	o := NewOracle()
	c := NewControlledExecutor()
	ex := &oracleExecutor{o, map[string]jobbatch.Executor{"controlled": c}}
	ctx := labTestContext(t)
	done := make(chan error, 1)
	go func() {
		_, err := ex.Start(ctx, jobbatch.Launch{ID: "R.1", Token: 1, Task: jobbatch.TaskSpec{Kind: "controlled"}}, func(jobbatch.Result) {})
		done <- err
	}()
	if _, err := c.AwaitStart(ctx, "R.1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseStart(ctx, 1, StartDecision{Decision: DecisionAcceptComplete, State: jobbatch.StateCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := labTestReceive(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	starts, accepted, _, _ := o.counters("R.1")
	if starts != 1 || accepted != 1 || o.maximum() != 1 {
		t.Fatal("physical acceptance interval")
	}
}
