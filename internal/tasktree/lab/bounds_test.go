package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

type labTestSpaces struct{}

func (labTestSpaces) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestCodecExactInputBound(t *testing.T) {
	scenario := labTestScenario()
	checkpoint := labTestRun(t, scenario, nil).Checkpoint
	for name, value := range map[string]any{"scenario": scenario, "checkpoint": checkpoint} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			for _, extra := range []int{0, 1} {
				reader := io.MultiReader(bytes.NewReader(data), io.LimitReader(labTestSpaces{}, int64(MaxInputBytes-len(data)+extra)))
				if name == "scenario" {
					_, err = ReadScenario(reader)
				} else {
					_, err = ReadCheckpoint(reader)
				}
				if (err != nil) != (extra == 1) {
					t.Fatalf("extra=%d: %v", extra, err)
				}
			}
		})
	}
}

func TestScenarioExactStepBound(t *testing.T) {
	scenario := labTestScenario()
	for i := 0; i < MaxScenarioSteps; i++ {
		scenario.Steps = append(scenario.Steps, labTestStep(fmt.Sprint(i), fmt.Sprint(i), `{"op":"view"}`))
	}
	if got := labTestRun(t, scenario, nil); len(got.Steps) != MaxScenarioSteps {
		t.Fatal("lost root views")
	}
	scenario.Steps = append(scenario.Steps, labTestStep("overflow", "overflow", `{"op":"view"}`))
	if got, err := Run(context.Background(), scenario, nil); err == nil || len(got.Steps) != 0 {
		t.Fatal("step overflow not prevalidated")
	}
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadScenario(bytes.NewReader(data)); err == nil {
		t.Fatal("codec accepted too many steps")
	}
}

func TestDirectScenarioSizeAndDuplicates(t *testing.T) {
	for _, payload := range []string{
		`{"op":"view","\u006fp":"view"}`,
		`{"op":"view","extra":{"x":0,"x":1}}`,
		`{"op":"view","padding":"` + strings.Repeat("x", MaxInputBytes) + `"}`,
	} {
		if got, err := Run(context.Background(), labTestScenario(labTestStep("bad", "bad", payload)), nil); err == nil || len(got.Steps) != 0 {
			t.Fatal("direct input not prevalidated")
		}
	}
	step := labTestStep("correctable", "correctable", `{"op":"not-an-operation"}`)
	step.Expect.IsError = labTestPtr(true)
	labTestRun(t, labTestScenario(step), nil)
	step.Payload = json.RawMessage(`{"op":`)
	labTestRun(t, labTestScenario(step), nil)
}

func TestDirectScenarioExactEncodedBound(t *testing.T) {
	scenario := labTestScenario(labTestStep("read", "read", `{"op":"view"}`))
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	scenario.Steps[0].Label += strings.Repeat("x", MaxInputBytes-len(data))
	labTestRun(t, scenario, nil)
	scenario.Steps[0].Label += "x"
	if report, err := Run(context.Background(), scenario, nil); err == nil || len(report.Steps) != 0 {
		t.Fatal("encoded overflow not rejected before execution")
	}
}

func TestRunRetainedBudgetRealRepeatedViews(t *testing.T) {
	items := make([]string, 31)
	for i := range items {
		items[i] = fmt.Sprintf("T%d", i)
	}
	payload, err := json.Marshal(map[string]any{"op": "init", "expected_revision": 0, "items": items})
	if err != nil {
		t.Fatal(err)
	}
	seed := labTestRun(t, labTestScenario(labTestStep("init", "init", string(payload))), nil).Checkpoint
	probe := labTestRun(t, labTestScenario(labTestStep("probe", "probe", `{"op":"view","id":"n0"}`)), &seed).Steps[0].Result
	if probe.Details.View == nil {
		t.Fatal("missing real view")
	}
	root := probe.Details.View.Root
	if count := 1 + len(root.Children); count != 32 {
		t.Fatalf("real view root+children count=%d, want 32", count)
	}
	for _, child := range root.Children {
		if len(child.Children) != 0 {
			t.Fatal("unexpected nested real view")
		}
	}
	if len(probe.Text) == 0 {
		t.Fatal("real view Text length=0")
	}
	accepted := min(MaxRetainedNodes/32, MaxRetainedTextBytes/len(probe.Text))
	if accepted < 1 || accepted >= MaxScenarioSteps {
		t.Fatalf("unusable real-view boundary %d", accepted)
	}
	scenario := labTestScenario()
	for i := 0; i < accepted; i++ {
		scenario.Steps = append(scenario.Steps, labTestStep(fmt.Sprint(i), fmt.Sprint(i), `{"op":"view","id":"n0"}`))
	}
	labTestRun(t, scenario, &seed)
	scenario.Steps = append(scenario.Steps, labTestStep("overflow", "overflow", `{"op":"view","id":"n0"}`))
	got, err := Run(context.Background(), scenario, &seed)
	if err == nil || len(got.Steps) != accepted {
		t.Fatalf("retention: report length=%d, want %d; error=%v", len(got.Steps), accepted, err)
	}
}

func TestRunRetainedTextRealRepeatedViews(t *testing.T) {
	scenario := labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["`+strings.Repeat("x", 2048)+`"]}`))
	scenario.Limits.MaxTitleBytes = 2048
	seed := labTestRun(t, scenario, nil).Checkpoint
	read := labTestStep("probe", "probe", `{"op":"view","id":"n0"}`)
	probeScenario := scenario
	probeScenario.Steps = []Step{read}
	probe := labTestRun(t, probeScenario, &seed).Steps[0].Result
	if len(probe.Text) == 0 {
		t.Fatal("real view Text length=0")
	}
	accepted := MaxRetainedTextBytes / len(probe.Text)
	if accepted < 1 || accepted >= MaxScenarioSteps || (accepted+1)*2 > MaxRetainedNodes {
		t.Fatalf("unusable text boundary %d", accepted)
	}
	probeScenario.Steps = nil
	for i := 0; i < accepted; i++ {
		probeScenario.Steps = append(probeScenario.Steps, labTestStep(fmt.Sprint(i), fmt.Sprint(i), string(read.Payload)))
	}
	labTestRun(t, probeScenario, &seed)
	probeScenario.Steps = append(probeScenario.Steps, labTestStep("overflow", "overflow", string(read.Payload)))
	got, err := Run(context.Background(), probeScenario, &seed)
	if err == nil || len(got.Steps) != accepted {
		t.Fatalf("text retention: report length=%d, want %d; error=%v", len(got.Steps), accepted, err)
	}
}

func TestScenarioBindingChecksRemainStrict(t *testing.T) {
	for name, change := range map[string]func(*Scenario){
		"schema": func(s *Scenario) { s.SchemaVersion = 2 },
		"key":    func(s *Scenario) { s.TreeKey = " " },
		"limits": func(s *Scenario) { s.Limits.MaxDepth = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			scenario := labTestScenario()
			change(&scenario)
			data, err := json.Marshal(scenario)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadScenario(bytes.NewReader(data)); err == nil {
				t.Fatal("codec accepted invalid binding")
			}
			if _, err := Run(context.Background(), scenario, nil); err == nil {
				t.Fatal("runner accepted invalid binding")
			}
		})
	}
	for _, data := range []string{`{"schema_version":1,"unknown":0}`, `{"schema_version":1,"limits":{"unknown":0}}`} {
		if _, err := ReadScenario(strings.NewReader(data)); err == nil {
			t.Fatal("accepted unknown field")
		}
	}
}

func TestCodecRecursiveDuplicates(t *testing.T) {
	scenario, err := json.Marshal(labTestScenario(labTestStep("read", "read", `{"op":"view"}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"schema_version":1`, `"schema_version":1,"schema_version":1`},
		{`"tree_key":"board"`, `"tree_key":"board","tree_key":"board"`},
		{`"max_nodes":50`, `"max_nodes":50,"max_nodes":50`},
		{`"is_error":false`, `"is_error":false,"\u0069s_error":false`},
		{`"op":"view"`, `"op":"view","op":"view"`},
	} {
		if _, err := ReadScenario(strings.NewReader(strings.Replace(string(scenario), pair[0], pair[1], 1))); err == nil {
			t.Fatalf("accepted duplicate %s", pair[0])
		}
	}
	checkpoint := labTestRun(t, labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`)), nil).Checkpoint
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"nodes":{`, `"nodes":{"n0":{},`},
		{`"receipts":{`, `"receipts":{"init":{},`},
		{`"title":"A"`, `"title":"A","\u0074itle":"A"`},
		{`"schema_version":1`, `"schema_version":1,"schema_version":1`},
		{`"tree_key":"board"`, `"tree_key":"board","tree_key":"board"`},
		{`"max_nodes":50`, `"max_nodes":50,"max_nodes":50`},
	} {
		_, err := ReadCheckpoint(strings.NewReader(strings.Replace(string(data), pair[0], pair[1], 1)))
		if err == nil {
			t.Fatalf("accepted duplicate %s", pair[0])
		}
	}
	if err := scanJSON([]byte(`{"a":{"x":1},"b":{"x":2}}`)); err != nil {
		t.Fatal(err)
	}
	if err := scanJSON([]byte(strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1))); err == nil {
		t.Fatal("accepted excessive depth")
	}
}

type labTestShortWriter struct{}

func (labTestShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestCheckpointShortWrite(t *testing.T) {
	checkpoint := labTestRun(t, labTestScenario(), nil).Checkpoint
	if err := WriteCheckpoint(labTestShortWriter{}, checkpoint); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}

func TestCheckpointSparseRevisionAndUninitializedGuard(t *testing.T) {
	initialized := labTestRun(t, labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`), labTestStep("done", "done", `{"op":"done","id":"n1","expected_revision":1}`)), nil).Checkpoint
	delete(initialized.Envelope.Receipts, "init")
	uninitialized := labTestRun(t, labTestScenario(), nil).Checkpoint
	uninitialized.Envelope.Revision = 7
	uninitialized.Envelope.Snapshot.TitleGuards = []tasktree.TitleGuard{{Kind: tasktree.KindTask, Title: "Guarded"}}
	for name, checkpoint := range map[string]Checkpoint{"sparse": initialized, "uninitialized guard": uninitialized} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteCheckpoint(&buf, checkpoint); err != nil {
				t.Fatal(err)
			}
			loaded, err := ReadCheckpoint(&buf)
			if err != nil || !reflect.DeepEqual(loaded, checkpoint) {
				t.Fatalf("roundtrip: %v", err)
			}
			read := labTestStep("read", "read", `{"op":"view"}`)
			read.Expect.Revision = labTestPtr(checkpoint.Envelope.Revision)
			report := labTestRun(t, labTestScenario(read), &loaded)
			if !reflect.DeepEqual(report.Checkpoint, checkpoint) {
				t.Fatal("read resume changed envelope")
			}
			if name == "sparse" {
				replay := labTestStep("done retry", "done", `{"op":"done","id":"n1","expected_revision":1}`)
				replay.Mode = "replay"
				labTestRun(t, labTestScenario(replay), &loaded)
			} else {
				guard := labTestStep("guard", "guard", `{"op":"init","expected_revision":7,"items":["Guarded"]}`)
				guard.Expect = Expectation{IsError: labTestPtr(true), ProblemCode: tasktree.CodeRemovedByOperator, Revision: labTestPtr(tasktree.Revision(7))}
				labTestRun(t, labTestScenario(guard), &loaded)
				init := labTestStep("init", "init", `{"op":"init","expected_revision":7,"items":["Other"]}`)
				init.Expect.Revision = labTestPtr(tasktree.Revision(8))
				labTestRun(t, labTestScenario(init), &loaded)
			}
		})
	}
}
