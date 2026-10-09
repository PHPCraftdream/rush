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
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func replTestOptions() REPLOptions {
	return REPLOptions{TreeKey: "board", Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}, Limits: labTestScenario().Limits}
}

func replTestLine(id, payload string) string {
	return `{"request_id":"` + id + `","payload":` + payload + "}\n"
}

func replTestResults(t *testing.T, buffer *bytes.Buffer) []protocol.ToolResult {
	t.Helper()
	decoder := json.NewDecoder(buffer)
	var results []protocol.ToolResult
	for {
		var result protocol.ToolResult
		err := decoder.Decode(&result)
		if err == io.EOF {
			return results
		}
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}
}

func TestREPLDurableReplayConflictAndContinue(t *testing.T) {
	options := replTestOptions()
	init := `{"op":"init","expected_revision":0,"items":["A","B"]}`
	input := replTestLine("init", init) +
		replTestLine("bad", `{"op":"done","id":"n1","expected_revision":0}`) +
		replTestLine("decode", `{"op":"unknown","actor":"operator"}`) +
		replTestLine("block", `{"op":"block","id":"n1","reason":"waiting","expected_revision":1}`) +
		replTestLine("init", `{ "items":["A","B"], "op":"init", "expected_revision":0 }`) +
		replTestLine("init", `{"op":"init","items":["changed"],"expected_revision":0}`) +
		replTestLine("view", `{"op":"view"}`)
	var output bytes.Buffer
	checkpoint, err := REPL(context.Background(), strings.NewReader(input), &output, options)
	if err != nil {
		t.Fatal(err)
	}
	results := replTestResults(t, &output)
	if len(results) != 7 {
		t.Fatalf("results=%d", len(results))
	}
	for index, code := range map[int]tasktree.ProblemCode{1: tasktree.CodeConflict, 2: tasktree.CodeInvalidInput, 5: tasktree.CodeRequestReused} {
		result := results[index]
		if !result.IsError || result.Details.Problem == nil || result.Details.Problem.Code != code || result.Details.Receipt != nil {
			t.Fatalf("result %d: %+v", index, result)
		}
	}
	if results[1].Details.Summary.Revision != 1 || results[2].Details.Summary.Revision != 1 || results[5].Details.Summary.Revision != 2 {
		t.Fatal("errors did not expose current revisions")
	}
	if results[0].IsError || results[3].IsError || results[4].IsError || results[6].IsError {
		t.Fatal("accepted operations reported errors")
	}
	if results[4].Details.Receipt == nil || !results[4].Details.Replayed || !reflect.DeepEqual(results[0].Details.Receipt, results[4].Details.Receipt) || results[4].Details.Summary.Revision != 2 {
		t.Fatal("replay lost original receipt or current summary")
	}
	if results[6].Details.View == nil || results[6].Details.Receipt != nil || results[6].Details.Summary.ActiveID != "n2" {
		t.Fatal("view must be read-only and show advanced focus")
	}
	envelope := checkpoint.Envelope
	if envelope.Revision != 2 || len(envelope.Receipts) != 2 || envelope.Snapshot.NextID != 3 || envelope.Snapshot.Nodes["n1"].Status != tasktree.Blocked || envelope.Snapshot.Nodes["n1"].Reason != "waiting" || envelope.Snapshot.Nodes["n2"].Status != tasktree.InProgress {
		t.Fatalf("durable state: %+v", envelope)
	}
	for _, id := range []tasktree.RequestID{"init", "block"} {
		if envelope.Receipts[id].Actor != options.Actor || envelope.Receipts[id].RequestID != id {
			t.Fatal("receipt authority/request identity was not fixed")
		}
	}
	var encoded bytes.Buffer
	if err := WriteCheckpoint(&encoded, checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadCheckpoint(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	options.Loaded = &loaded
	output.Reset()
	resumed, err := REPL(context.Background(), strings.NewReader(replTestLine("init", init)+replTestLine("add", `{"op":"add","expected_revision":2,"items":["C"]}`)), &output, options)
	if err != nil {
		t.Fatal(err)
	}
	results = replTestResults(t, &output)
	if len(results) != 2 || !results[0].Details.Replayed || results[0].Details.Receipt == nil || results[0].Details.Receipt.CommittedRevision != 1 || resumed.Envelope.Revision != 3 || resumed.Envelope.Snapshot.NextID != 4 || resumed.Envelope.Snapshot.Nodes["n3"].Title != "C" || len(resumed.Envelope.Receipts) != 3 {
		t.Fatal("restart did not preserve receipts/counter and accept new mutation")
	}
}

func TestREPLStrictWrapperStopsBeforeFollowingMutation(t *testing.T) {
	bad := []string{
		`null`, `[]`, `{`, `quit`, `help`,
		`{"request_id":"r"}`, `{"payload":{"op":"view"}}`,
		`{"request_id":" ","payload":{"op":"view"}}`,
		`{"request_id":7,"payload":{"op":"view"}}`,
		`{"request_id":"r","payload":null}`,
		`{"request_id":"r","payload":[]}`,
		`{"request_id":"r","payload":{"op":"view"}} {}`,
		`{"request_id":"r","request_id":"s","payload":{"op":"view"}}`,
		`{"request_id":"r","payload":{"op":"view"},"payload":{"op":"view"}}`,
	}
	for _, key := range []string{"actor", "actor_id", "tree_key", "invocation", "model", "authority", "Request_ID"} {
		bad = append(bad, `{"request_id":"r","payload":{"op":"view"},"`+key+`":"operator"}`)
	}
	for _, line := range bad {
		t.Run(line, func(t *testing.T) {
			var output bytes.Buffer
			checkpoint, err := REPL(context.Background(), strings.NewReader(line+"\n"+replTestLine("init", `{"op":"init","items":["A"],"expected_revision":0}`)), &output, replTestOptions())
			if err == nil || output.Len() != 0 || checkpoint.Envelope.Snapshot.Initialized {
				t.Fatal("malformed wrapper was executed or scanning continued")
			}
		})
	}
}

func TestREPLEOFEmptyAndReadOnlyState(t *testing.T) {
	for _, input := range []string{"", " \t\n" + replTestLine("init", `{"op":"init","items":["A"],"expected_revision":0}`), replTestLine("view", `{"op":"view"}`), strings.TrimSuffix(replTestLine("view", `{"op":"view"}`), "\n")} {
		var output bytes.Buffer
		checkpoint, err := REPL(context.Background(), strings.NewReader(input), &output, replTestOptions())
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.Envelope.Revision != 0 || len(checkpoint.Envelope.Receipts) != 0 || checkpoint.Envelope.Snapshot.Initialized || checkpoint.Envelope.Snapshot.NextID != 1 {
			t.Fatal("termination/read mutated missing board")
		}
		results := replTestResults(t, &output)
		want := 0
		if strings.HasPrefix(input, "{") {
			want = 1
		}
		if len(results) != want || (want == 1 && (results[0].IsError || results[0].Details.View == nil)) {
			t.Fatal("EOF/empty line did not delimit the JSONL session")
		}
	}
}

type replTestErrorReader struct{}

func (replTestErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestREPLPropagatesStreamContextAndBindingFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		reader io.Reader
		writer io.Writer
		want   error
	}{
		{"reader", context.Background(), replTestErrorReader{}, io.Discard, io.ErrUnexpectedEOF},
		{"reader after result", context.Background(), io.MultiReader(strings.NewReader(replTestLine("view", `{"op":"view"}`)), replTestErrorReader{}), io.Discard, io.ErrUnexpectedEOF},
		{"writer", context.Background(), strings.NewReader(replTestLine("init", `{"op":"init","expected_revision":0,"items":["A"]}`)), labTestFailWriter{}, io.ErrClosedPipe},
		{"context", ctx, strings.NewReader(""), io.Discard, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := REPL(tc.ctx, tc.reader, tc.writer, replTestOptions())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	var output bytes.Buffer
	if _, err := REPL(context.Background(), strings.NewReader(strings.Repeat("x", MaxREPLLineBytes+1)), &output, replTestOptions()); err == nil || output.Len() != 0 {
		t.Fatal("oversized transport line was accepted")
	}
	checkpoint := labTestRun(t, labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`)), nil).Checkpoint
	for _, change := range []func(*REPLOptions){
		func(o *REPLOptions) { o.TreeKey = "other" },
		func(o *REPLOptions) { o.Limits.MaxNodes++ },
		func(o *REPLOptions) { o.Actor.Kind = "model" },
		func(o *REPLOptions) { o.Actor.ID = " " },
	} {
		options := replTestOptions()
		options.Loaded = &checkpoint
		change(&options)
		output.Reset()
		if _, err := REPL(context.Background(), strings.NewReader(replTestLine("view", `{"op":"view"}`)), &output, options); err == nil || output.Len() != 0 {
			t.Fatal("invalid fixed binding accepted")
		}
	}
}
