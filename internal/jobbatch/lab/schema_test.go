package lab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func schemaTestDecode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON: %v", err)
	}
	return nil
}

func schemaTestRoundTrip(t *testing.T, value any, dst any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaTestDecode(data, dst); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, reflect.ValueOf(dst).Elem().Interface()) {
		t.Fatalf("round trip differs: %s", data)
	}
}

func TestSchemaFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/scenarios/*.json")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields("c-a c-a-ii c-b c-b-mirror c-c c-d c-d-i c-d-ii c-d-iii c-e c-e-undeclared f-d r-a r-a-h-held r-a-h-blocked r-b r-c-empty r-c-pending r-d-i r-d-ii r3-add r3-add-helper r3-chain r3-one v-b v-c v-c-empty-code v-c-result-code v-c-kept-code v-e-task v-e-batch w-a w-b-i w-b-ii w-d")
	if len(files) != len(want) {
		t.Fatalf("fixtures: got %d want %d", len(files), len(want))
	}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join("testdata", "scenarios", name+".json")); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var s Scenario
			if err := schemaTestDecode(data, &s); err != nil {
				t.Fatal(err)
			}
			var copy Scenario
			schemaTestRoundTrip(t, s, &copy)
			if s.Version != 1 || s.WaitBoundMS != 10000 || s.Expect == nil {
				t.Fatal("missing contract bound/oracle")
			}
			if s.Expect.Roots == nil || s.Expect.Nodes == nil || s.Expect.Notices == nil || s.Expect.ExitCode == nil {
				t.Fatal("smoke oracle requires roots, nodes, notices and exit_code")
			}
			for _, root := range *s.Expect.Roots {
				if _, ok := (*s.Expect.Notices)[root.ID]; !ok {
					t.Fatalf("missing notice oracle for root %s", root.ID)
				}
			}
			refs := map[string]bool{}
			async := map[string]ActionName{}
			for _, a := range s.Script {
				if !schemaTestKnownAction(a.Action) {
					t.Fatalf("unknown fixture action %q", a.Action)
				}
				if a.Async != nil {
					async[*a.Async] = a.Action
				}
				if a.Action == ActionJoin {
					if a.Name == nil {
						t.Fatal("unnamed join")
					}
					if _, ok := async[*a.Name]; !ok {
						t.Fatal("unknown async join")
					}
				}
				if a.Action == ActionSubmit || a.Action == ActionAdd {
					if a.Ref == nil || *a.Ref == "" || !jobbatch.ValidRef(*a.Ref) || refs[*a.Ref] {
						t.Fatal("invalid ref")
					}
					refs[*a.Ref] = true
				}
				if strings.HasPrefix(string(a.Action), "await_") && (a.TimeoutMS == nil || *a.TimeoutMS <= 0) {
					t.Fatal("unbounded await")
				}
			}
			last := s.Script[len(s.Script)-1]
			closed := last.Action == ActionClose && last.Async == nil && last.Expect != nil && (*last.Expect == CloseOK || *last.Expect == CloseTimeout || *last.Expect == CloseDispose)
			joined := last.Action == ActionJoin && last.Name != nil && async[*last.Name] == ActionClose
			if !closed && !joined {
				t.Fatal("missing final close or joined async close")
			}
			if strings.HasPrefix(filepath.Base(file), "r3-") {
				if !closed || *last.Expect != CloseOK {
					t.Fatal("R3 requires synchronous CloseOK")
				}
				schemaTestR3(t, filepath.Base(file), s)
			}
		})
	}
}

func schemaTestR3(t *testing.T, file string, s Scenario) {
	t.Helper()
	name := strings.TrimSuffix(file, ".json")
	if *s.Script[0].Ref != name+"-R" {
		t.Fatal("incorrect R3 submit ref")
	}
	notices := (*s.Expect.Notices)["R"]
	want := []jobbatch.ID{"R"}
	if name == "r3-add" {
		want = []jobbatch.ID{"R.1", "R.2", "R"}
	}
	var got []jobbatch.ID
	for _, n := range notices {
		got = append(got, n.Node)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notices %v, want %v", got, want)
	}
	if strings.HasPrefix(name, "r3-add") {
		if *s.Script[4].Ref != name+"-second" || s.Script[4].Action != ActionAdd {
			t.Fatal("add ref/fence")
		}
		if s.Script[3].Action != ActionAwaitState || *s.Script[3].State != jobbatch.StateRunning || s.Script[6].Action != ActionAwaitState || *s.Script[6].State != jobbatch.StateCompleted {
			t.Fatal("missing fences")
		}
	}
}

func TestSchemaUnknownFields(t *testing.T) {
	for _, data := range []string{
		`{"id":"invented"}`, `{"policy":{}}`,
		`{"runner":{"unknown":0}}`, `{"payloads":{"p":{"unknown":0}}}`,
		`{"script":[{"unknown":0}]}`, `{"script":[{"spec":{"task":{"policy":"each"}}}]}`,
		`{"script":[{"spec":{"batch":{"payload_ref":"p"}}}]}`,
		`{"expect":{"nodes":[{"id":"R","unknown":0}]}}`,
		`{"expect":{"unknown":0}}`, `{} {}`,
	} {
		var s Scenario
		if schemaTestDecode([]byte(data), &s) == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestSchemaPresence(t *testing.T) {
	data := []byte(`{"expect":{"roots":[],"nodes":[{"id":"R","parent":"","cause":"","state":"","outcome":{"state":"","code":""},"control":"","control_pending":false,"attempts":0,"starts":0,"accepted":0,"stops":0,"disposes":0}],"notices":{},"max_active":0,"before":[],"exit_code":0},"script":[{"action":"expect_output","timeout_ms":0,"async":"","expect_error":"","code":"","eof":false,"contains":"","call":0,"items":[],"group":""}]}`)
	var s Scenario
	if err := schemaTestDecode(data, &s); err != nil {
		t.Fatal(err)
	}
	var copy Scenario
	schemaTestRoundTrip(t, s, &copy)
	encoded, _ := json.Marshal(s)
	for _, fragment := range []string{`"roots":[]`, `"notices":{}`, `"max_active":0`, `"before":[]`, `"exit_code":0`, `"eof":false`, `"code":""`, `"starts":0`, `"control_pending":false`} {
		if !bytes.Contains(encoded, []byte(fragment)) {
			t.Fatalf("lost presence %s", fragment)
		}
	}
	var absent Scenario
	if err := schemaTestDecode([]byte(`{}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Expect != nil {
		t.Fatal("invented oracle")
	}
}

func schemaTestKnownAction(name ActionName) bool {
	switch name {
	case ActionSubmit, ActionAdd, ActionStop, ActionInject, ActionCapacityAvailable,
		ActionRelease, ActionClose, ActionAwaitStart, ActionReleaseStart, ActionComplete,
		ActionEmit, ActionAwaitInject, ActionHoldStop, ActionFailStop, ActionAwaitStop,
		ActionReturnStop, ActionHoldDispose, ActionAwaitDispose, ActionReturnDispose,
		ActionHoldRead, ActionReturnRead, ActionAwaitState, ActionExpectOutput, ActionJoin,
		ActionAwaitRecord:
		return true
	}
	return false
}

func TestSchemaActionNames(t *testing.T) {
	got := []ActionName{ActionSubmit, ActionAdd, ActionStop, ActionInject, ActionCapacityAvailable, ActionRelease, ActionClose, ActionAwaitStart, ActionReleaseStart, ActionComplete, ActionEmit, ActionAwaitInject, ActionHoldStop, ActionFailStop, ActionAwaitStop, ActionReturnStop, ActionHoldDispose, ActionAwaitDispose, ActionReturnDispose, ActionHoldRead, ActionReturnRead, ActionAwaitState, ActionExpectOutput, ActionJoin, ActionAwaitRecord}
	want := strings.Fields("submit add stop inject capacity_available release close await_start release_start complete emit await_inject hold_stop fail_stop await_stop return_stop hold_dispose await_dispose return_dispose hold_read return_read await_state expect_output join await_record")
	for i, name := range got {
		if string(name) != want[i] {
			t.Fatalf("action %d: %s", i, name)
		}
		a := Action{Action: name}
		var copy Action
		schemaTestRoundTrip(t, a, &copy)
	}
	// Data decoding is not name validation; the future loader must reject these.
	for _, name := range []ActionName{"await_read", "rush", "unknown"} {
		if schemaTestKnownAction(name) {
			t.Fatalf("accepted unknown action %q", name)
		}
		for _, known := range got {
			if name == known {
				t.Fatal("invented action")
			}
		}
	}
	if DecisionAccept != "accept" || DecisionAcceptComplete != "accept_complete" || DecisionRefuseCapacity != "refuse_capacity" || DecisionFail != "fail" || GateOK != "ok" || GatePanic != "panic" {
		t.Fatal("decision contract")
	}
	if []int{ExitSuccess, ExitFailed, ExitInvalid, ExitUnfinished, ExitMismatch, ExitIncompleteJournal}[5] != 5 {
		t.Fatal("exit contract")
	}
}

func TestSchemaErrorNames(t *testing.T) {
	cases := []struct {
		err     error
		name    ErrorName
		literal string
	}{
		{jobbatch.ErrInvalidSpec, ErrorInvalidSpec, "invalid_spec"},
		{jobbatch.ErrInvalidEvent, ErrorInvalidEvent, "invalid_event"},
		{jobbatch.ErrDuplicateRoot, ErrorDuplicateRoot, "duplicate_root"},
		{jobbatch.ErrUnknownNode, ErrorUnknownNode, "unknown_node"},
		{jobbatch.ErrTerminal, ErrorTerminal, "terminal"},
		{jobbatch.ErrNotTerminal, ErrorNotTerminal, "not_terminal"},
		{jobbatch.ErrNotStarted, ErrorNotStarted, "not_started"},
		{jobbatch.ErrUnsupported, ErrorUnsupported, "unsupported"},
		{jobbatch.ErrReleased, ErrorReleased, "released"},
		{jobbatch.ErrClosing, ErrorClosing, "closing"},
		{jobbatch.ErrClosed, ErrorClosed, "closed"},
		{jobbatch.ErrCloseTimeout, ErrorCloseTimeout, "close_timeout"},
		{jobbatch.ErrGap, ErrorGap, "gap"},
		{jobbatch.ErrExecutorPanic, ErrorExecutorPanic, "executor_panic"},
		{jobbatch.ErrExecutionDone, ErrorExecutionDone, "execution_done"},
		{jobbatch.ErrDispose, ErrorDispose, "dispose"},
		{jobbatch.ErrCapacity, ErrorCapacity, "capacity"},
		{ErrUnknownAttempt, ErrorUnknownAttempt, "unknown_attempt"},
		{ErrAttemptPhase, ErrorAttemptPhase, "attempt_phase"},
	}
	for _, c := range cases {
		if string(c.name) != c.literal {
			t.Fatal("error spelling")
		}
		for _, err := range []error{c.err, fmt.Errorf("wrapped: %w", c.err)} {
			if got := ErrorNameOf(err); got != c.name {
				t.Fatalf("got %s want %s", got, c.name)
			}
		}
	}
	for _, err := range []error{nil, errors.New("terminal"), errors.New("batchlab: unknown attempt")} {
		if ErrorNameOf(err) != "" {
			t.Fatal("unknown error classified")
		}
	}
	if ErrorNameOf(errors.Join(jobbatch.ErrTerminal, jobbatch.ErrDispose, jobbatch.ErrCloseTimeout)) != ErrorCloseTimeout {
		t.Fatal("timeout precedence")
	}
	if ErrorNameOf(errors.Join(jobbatch.ErrTerminal, jobbatch.ErrDispose)) != ErrorDispose {
		t.Fatal("dispose precedence")
	}
	if ErrorNameOf(&jobbatch.DisposeError{}) != ErrorDispose || ErrorNameOf(&jobbatch.CloseTimeoutError{}) != ErrorCloseTimeout {
		t.Fatal("typed carriers")
	}
}

func TestSchemaJournal(t *testing.T) {
	zero := uint64(0)
	footer := JournalFooter{Type: "footer", FinalCursor: &zero, Capture: JournalCapture{Status: CaptureComplete}, Outcome: JournalOutcome{Status: OutcomeClosed}, Roots: []JournalRoot{{ID: "R", SettledSequence: &zero}}, Inputs: []JournalInput{}}
	var copy JournalFooter
	schemaTestRoundTrip(t, footer, &copy)
	if copy.FinalCursor == nil || copy.Roots[0].SettledSequence == nil {
		t.Fatal("lost boundary presence")
	}
	footer.FinalCursor = nil
	footer.Roots[0].SettledSequence = nil
	encoded, _ := json.Marshal(footer)
	if bytes.Contains(encoded, []byte(`"final_cursor"`)) || bytes.Contains(encoded, []byte(`"settled_sequence"`)) {
		t.Fatal("invented boundary")
	}
	header := JournalHeader{Type: "header", Format: "batchlab", Version: 1}
	var h JournalHeader
	schemaTestRoundTrip(t, header, &h)
}
