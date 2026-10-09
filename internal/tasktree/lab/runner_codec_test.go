package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func labTestPtr[T any](value T) *T { return &value }

func labTestScenario(steps ...Step) Scenario {
	return Scenario{SchemaVersion: 1, TreeKey: "board", Limits: tasktree.Limits{
		MaxNodes: 50, MaxDepth: 8, MaxTitleBytes: 100, MaxReasonBytes: 100, MaxTombstones: 50, MaxReceipts: 50,
	}, Steps: steps}
}

func labTestStep(label, id, payload string) Step {
	return Step{Label: label, RequestID: tasktree.RequestID(id), Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}, Payload: json.RawMessage(payload), Expect: Expectation{IsError: labTestPtr(false)}}
}

func labTestRun(t *testing.T, scenario Scenario, checkpoint *Checkpoint) Report {
	t.Helper()
	report, err := Run(context.Background(), scenario, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestRunnerFailedExpectations(t *testing.T) {
	base := labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A","B"]}`)
	cases := map[string]Expectation{
		"error":         {IsError: labTestPtr(true)},
		"revision":      {IsError: labTestPtr(false), Revision: labTestPtr(tasktree.Revision(99))},
		"receipt":       {IsError: labTestPtr(false), ReceiptRevision: labTestPtr(tasktree.Revision(99))},
		"replayed":      {IsError: labTestPtr(false), Replayed: labTestPtr(true)},
		"problem":       {IsError: labTestPtr(false), ProblemCode: tasktree.CodeConflict},
		"active":        {IsError: labTestPtr(false), ActiveID: labTestPtr(tasktree.NodeID("n2"))},
		"next":          {IsError: labTestPtr(false), NextID: labTestPtr(tasktree.NodeID("n2"))},
		"full progress": {IsError: labTestPtr(false), Progress: &tasktree.Progress{Total: 2, Pending: 1, InProgress: 1, Actionable: 2, Unfinished: 2, AllCompleted: true}},
		"node":          {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", Title: labTestPtr("wrong")}}},
		"parent":        {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", ParentID: labTestPtr(tasktree.NodeID("n2"))}}},
		"kind":          {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", Kind: labTestPtr(tasktree.KindGroup)}}},
		"status":        {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", Status: labTestPtr(tasktree.Pending)}}},
		"reason":        {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", Reason: labTestPtr("wrong")}}},
		"active form":   {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n1", ActiveForm: labTestPtr("wrong")}}},
		"missing node":  {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n99"}}},
		"order":         {IsError: labTestPtr(false), Nodes: []NodeExpectation{{ID: "n0", Children: labTestPtr([]tasktree.NodeID{"n2", "n1"})}}},
		"removed":       {IsError: labTestPtr(false), RemovedIDs: []tasktree.NodeID{"n1"}},
		"absent":        {IsError: labTestPtr(false), AbsentIDs: []tasktree.NodeID{"n1"}},
	}
	for name, expectation := range cases {
		t.Run(name, func(t *testing.T) {
			step := base
			step.Expect = expectation
			if _, err := Run(context.Background(), labTestScenario(step), nil); err == nil {
				t.Fatal("accepted failed expectation")
			}
		})
	}
}

func TestRunnerDuplicateModes(t *testing.T) {
	init := labTestStep("init", "r1", `{"op":"init","expected_revision":0,"items":["A"]}`)
	replay := labTestStep("retry", "r1", `{ "items": ["A"], "expected_revision": 0, "op": "init" }`)
	replay.Mode = "replay"
	replay.Expect.Replayed = labTestPtr(true)
	replay.Expect.ReceiptRevision = labTestPtr(tasktree.Revision(1))
	labTestRun(t, labTestScenario(init, replay), nil)
	canonical := replay
	canonical.Payload = json.RawMessage(`{"op":"init","expected_revision":0,"list":[{"kind":"task","title":"A"}]}`)
	labTestRun(t, labTestScenario(init, canonical), nil)
	for name, change := range map[string]func(*Step){
		"actor":    func(s *Step) { s.Actor.ID = "other" },
		"payload":  func(s *Step) { s.Payload = json.RawMessage(`{"op":"init","expected_revision":0,"items":["B"]}`) },
		"revision": func(s *Step) { s.Payload = json.RawMessage(`{"op":"init","expected_revision":1,"items":["A"]}`) },
	} {
		t.Run(name, func(t *testing.T) {
			reuse := replay
			change(&reuse)
			if _, err := Run(context.Background(), labTestScenario(init, reuse), nil); err == nil {
				t.Fatal("mismatch accepted as replay")
			}
			reuse.Mode = "reuse"
			reuse.Expect = Expectation{IsError: labTestPtr(true), ProblemCode: tasktree.CodeRequestReused}
			labTestRun(t, labTestScenario(init, reuse), nil)
		})
	}
	falseReuse := replay
	falseReuse.Mode = "reuse"
	if _, err := Run(context.Background(), labTestScenario(init, falseReuse), nil); err == nil {
		t.Fatal("exact match accepted as reuse")
	}
	duplicate := replay
	duplicate.Mode = ""
	if _, err := Run(context.Background(), labTestScenario(init, duplicate), nil); err == nil {
		t.Fatal("implicit duplicate accepted")
	}
	view := labTestStep("view", "read", `{"op":"view"}`)
	labTestRun(t, labTestScenario(init, view), nil)
	readReplay := view
	readReplay.Label, readReplay.Mode = "read retry", "replay"
	if _, err := Run(context.Background(), labTestScenario(view, readReplay), nil); err == nil {
		t.Fatal("read treated as durable replay")
	}
}

func TestRunnerPrevalidationAndContext(t *testing.T) {
	step := labTestStep("view", "read", `{"op":"view"}`)
	for name, scenario := range map[string]Scenario{
		"labels":            labTestScenario(step, step),
		"missing error":     labTestScenario(Step{Label: "missing", RequestID: "missing"}),
		"missing duplicate": labTestScenario(func() Step { s := step; s.Mode = "replay"; return s }()),
		"unknown mode":      labTestScenario(func() Step { s := step; s.Mode = "retry"; return s }()),
	} {
		t.Run(name, func(t *testing.T) {
			report, err := Run(context.Background(), scenario, nil)
			if err == nil || len(report.Steps) != 0 {
				t.Fatal("scenario not prevalidated")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, labTestScenario(step), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCheckpointRestart(t *testing.T) {
	init := labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A","B","C"]}`)
	block := labTestStep("block", "block", `{"op":"block","id":"n1","reason":"waiting","expected_revision":1}`)
	remove := labTestStep("remove", "remove", `{"op":"rm","id":"n3","expected_revision":2}`)
	remove.Actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
	first := labTestRun(t, labTestScenario(init, block, remove), nil)
	var buf bytes.Buffer
	if err := WriteCheckpoint(&buf, first.Checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadCheckpoint(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, first.Checkpoint) {
		t.Fatal("codec lost envelope data")
	}
	replay := init
	replay.Label, replay.Mode = "restart retry", "replay"
	replay.Expect = Expectation{IsError: labTestPtr(false), Revision: labTestPtr(tasktree.Revision(3)), ReceiptRevision: labTestPtr(tasktree.Revision(1)), Replayed: labTestPtr(true), Nodes: []NodeExpectation{
		{ID: "n0", Children: labTestPtr([]tasktree.NodeID{"n1", "n2"})},
		{ID: "n1", Status: labTestPtr(tasktree.Blocked), Reason: labTestPtr("waiting")},
	}, RemovedIDs: []tasktree.NodeID{"n3"}, AbsentIDs: []tasktree.NodeID{"n4"}}
	replay.Expect.Progress = &tasktree.Progress{Total: 2, InProgress: 1, Blocked: 1, Actionable: 1, Unfinished: 2}
	guard := labTestStep("guard", "guard", `{"op":"add","expected_revision":3,"items":["C"]}`)
	guard.Expect = Expectation{IsError: labTestPtr(true), ProblemCode: tasktree.CodeRemovedByOperator, Revision: labTestPtr(tasktree.Revision(3))}
	add := labTestStep("add", "add", `{"op":"add","expected_revision":3,"items":["D"]}`)
	add.Expect.Nodes = []NodeExpectation{{ID: "n4", Title: labTestPtr("D")}, {ID: "n0", Children: labTestPtr([]tasktree.NodeID{"n1", "n2", "n4"})}}
	final := labTestRun(t, labTestScenario(replay, guard, add), &loaded)
	if final.Checkpoint.Envelope.Snapshot.NextID != 5 || len(final.Checkpoint.Envelope.Receipts) != 4 {
		t.Fatal("counter or receipts lost on restart")
	}
	if len(final.Steps[0].Result.Details.Created) != 3 || !final.Steps[0].Result.Details.Created[2].Removed {
		t.Fatal("replay resurrected removed created node")
	}
	implicit := replay
	implicit.Mode = ""
	if _, err := Run(context.Background(), labTestScenario(implicit), &loaded); err == nil {
		t.Fatal("loaded receipt did not count as duplicate")
	}
	for _, mismatch := range []Checkpoint{
		func() Checkpoint { c := loaded; c.TreeKey = "other"; return c }(),
		func() Checkpoint { c := loaded; c.Limits.MaxNodes++; return c }(),
	} {
		if _, err := Run(context.Background(), labTestScenario(), &mismatch); err == nil {
			t.Fatal("accepted mismatched checkpoint")
		}
	}
}

func TestCheckpointRejectsCorruption(t *testing.T) {
	checkpoint := labTestRun(t, labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`)), nil).Checkpoint
	for name, corrupt := range map[string]func(*Checkpoint){
		"counter": func(c *Checkpoint) { c.Envelope.Snapshot.NextID = 1 },
		"parent": func(c *Checkpoint) {
			n := c.Envelope.Snapshot.Nodes["n1"]
			n.ParentID = "n99"
			c.Envelope.Snapshot.Nodes["n1"] = n
		},
		"receipt": func(c *Checkpoint) {
			r := c.Envelope.Receipts["init"]
			r.CommittedRevision = 2
			c.Envelope.Receipts["init"] = r
		},
		"schema": func(c *Checkpoint) { c.SchemaVersion = 2 },
		"limits": func(c *Checkpoint) { c.Limits.MaxReceipts = 0 },
		"key":    func(c *Checkpoint) { c.TreeKey = " " },
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			var bad Checkpoint
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
			corrupt(&bad)
			var out bytes.Buffer
			if err := WriteCheckpoint(&out, bad); err == nil || out.Len() != 0 {
				t.Fatal("wrote corrupt checkpoint")
			}
			data, err = json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadCheckpoint(bytes.NewReader(data)); err == nil {
				t.Fatal("read corrupt checkpoint")
			}
			if _, err := Run(context.Background(), labTestScenario(), &bad); err == nil {
				t.Fatal("seeded corrupt checkpoint")
			}
		})
	}
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"null", "[]", string(data) + " {}", string(data) + " garbage", strings.Replace(string(data), `"next_id":2`, `"next_id":2,"unexpected":1`, 1)} {
		if _, err := ReadCheckpoint(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if err := WriteCheckpoint(labTestFailWriter{}, checkpoint); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error: %v", err)
	}
}

type labTestFailWriter struct{}

func (labTestFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReadScenarioStrict(t *testing.T) {
	scenario := labTestScenario(labTestStep("read", "read", `{"op":"view"}`))
	data, err := json.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadScenario(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"null", string(data) + " true", strings.Replace(string(data), `"label":"read"`, `"label":"read","unexpected":1`, 1), strings.Replace(string(data), `"is_error":false`, `"is_error":null`, 1)} {
		if _, err := ReadScenario(strings.NewReader(input)); err == nil {
			t.Fatal("accepted malformed scenario")
		}
	}
}
