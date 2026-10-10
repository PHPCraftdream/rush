package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func cliTestPtr[T any](value T) *T { return &value }

func cliTestScenario(steps ...lab.Step) lab.Scenario {
	return lab.Scenario{SchemaVersion: 1, TreeKey: "board", Limits: tasktree.Limits{
		MaxNodes: 50, MaxDepth: 8, MaxTitleBytes: 100, MaxReasonBytes: 100, MaxTombstones: 50, MaxReceipts: 50,
	}, Steps: steps}
}

func cliTestStep(label, id, payload string) lab.Step {
	return lab.Step{Label: label, RequestID: tasktree.RequestID(id), Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}, Payload: json.RawMessage(payload), Expect: lab.Expectation{IsError: cliTestPtr(false)}}
}

func cliTestJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliTestRun(t *testing.T, args []string, input string, want int) []byte {
	t.Helper()
	var output, diagnostics bytes.Buffer
	if code := runCLI(args, strings.NewReader(input), &output, &diagnostics); code != want {
		t.Fatalf("exit=%d want=%d diagnostics=%s", code, want, &diagnostics)
	}
	return output.Bytes()
}

func cliTestCheckpoint(t *testing.T, path string) lab.Checkpoint {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := lab.ReadCheckpoint(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func cliTestDecode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("trailing output: %v", err)
	}
	return value
}

func TestCLIRunSnapshotInspectResumeDurableEnvelope(t *testing.T) {
	dir := t.TempDir()
	scenarioPath := filepath.Join(dir, "scenario.json")
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	init := cliTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A","B","C"]}`)
	block := cliTestStep("block", "block", `{"op":"block","id":"n1","reason":"waiting","expected_revision":1}`)
	remove := cliTestStep("remove", "remove", `{"op":"rm","id":"n3","expected_revision":2}`)
	remove.Actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator-a"}
	cliTestJSONFile(t, scenarioPath, cliTestScenario(init, block, remove))
	report := cliTestDecode[lab.Report](t, cliTestRun(t, []string{"run", "--snapshot", checkpointPath, scenarioPath}, "", 0))
	checkpoint := cliTestCheckpoint(t, checkpointPath)
	if len(report.Steps) != 3 || !reflect.DeepEqual(report.Checkpoint, checkpoint) {
		t.Fatal("run output and exported full checkpoint disagree")
	}
	envelope := checkpoint.Envelope
	if envelope.Revision != 3 || len(envelope.Receipts) != 3 || envelope.Snapshot.NextID != 4 || envelope.Snapshot.Nodes["n1"].Reason != "waiting" || envelope.Snapshot.Nodes["n1"].Status != tasktree.Blocked || envelope.Snapshot.Nodes["n2"].Status != tasktree.InProgress || !reflect.DeepEqual(envelope.Snapshot.Nodes["n0"].Children, []tasktree.NodeID{"n1", "n2"}) {
		t.Fatal("export lost revisions, receipts, blockers, ordering or counter")
	}
	if envelope.Snapshot.Tombstones["n3"].Actor != remove.Actor || len(envelope.Snapshot.TitleGuards) != 1 || envelope.Receipts["init"].CommittedRevision != 1 {
		t.Fatal("export lost deletion authority/guards or original receipt")
	}
	inspected := cliTestDecode[lab.Checkpoint](t, cliTestRun(t, []string{"inspect", checkpointPath}, "", 0))
	if !reflect.DeepEqual(inspected, checkpoint) {
		t.Fatal("inspect printed only a projection instead of full envelope")
	}
	replay := init
	replay.Label, replay.Mode = "restart retry", "replay"
	replay.Expect = lab.Expectation{IsError: cliTestPtr(false), Revision: cliTestPtr(tasktree.Revision(3)), ReceiptRevision: cliTestPtr(tasktree.Revision(1)), Replayed: cliTestPtr(true)}
	guard := cliTestStep("guard", "guard", `{"op":"add","expected_revision":3,"items":["C"]}`)
	guard.Expect = lab.Expectation{IsError: cliTestPtr(true), ProblemCode: tasktree.CodeRemovedByOperator, Revision: cliTestPtr(tasktree.Revision(3))}
	add := cliTestStep("add", "add", `{"op":"add","expected_revision":3,"items":["D"]}`)
	cliTestJSONFile(t, scenarioPath, cliTestScenario(replay, guard, add))
	report = cliTestDecode[lab.Report](t, cliTestRun(t, []string{"run", "--load", checkpointPath, "--snapshot", checkpointPath, scenarioPath}, "", 0))
	resumed := cliTestCheckpoint(t, checkpointPath)
	original := envelope.Receipts["init"]
	if !reflect.DeepEqual(report.Checkpoint, resumed) || len(report.Steps) != 3 || !report.Steps[0].Result.Details.Replayed || !reflect.DeepEqual(report.Steps[0].Result.Details.Receipt, &original) || !report.Steps[1].Result.IsError || report.Steps[1].Result.Details.Problem == nil || report.Steps[1].Result.Details.Problem.Code != tasktree.CodeRemovedByOperator {
		t.Fatal("resume lost replay or expected error boundary")
	}
	if resumed.Envelope.Revision != 4 || len(resumed.Envelope.Receipts) != 4 || resumed.Envelope.Snapshot.NextID != 5 || resumed.Envelope.Snapshot.Nodes["n4"].Title != "D" || resumed.Envelope.Snapshot.Nodes["n1"].Reason != "waiting" || !reflect.DeepEqual(resumed.Envelope.Snapshot.Nodes["n0"].Children, []tasktree.NodeID{"n1", "n2", "n4"}) || len(resumed.Envelope.Snapshot.Tombstones) != 1 || len(resumed.Envelope.Snapshot.TitleGuards) != 1 {
		t.Fatal("resume duplicated nodes or lost durable envelope data")
	}
	if _, exists := resumed.Envelope.Receipts["guard"]; exists {
		t.Fatal("correctable rejection acquired durable receipt")
	}
}

func TestCLIUsageHelpAndFlagOrderingExits(t *testing.T) {
	for _, args := range [][]string{
		nil, {"unknown"}, {"run"}, {"inspect"}, {"inspect", "a", "b"},
		{"run", "scenario", "--load", "checkpoint"}, {"run", "scenario", "--help"}, {"inspect", "checkpoint", "-h"}, {"repl", "positional", "--help"}, {"run", "--unknown"},
		{"repl"}, {"repl", "--tree-key", "board", "extra"},
		{"repl", "--tree-key", "board", "--actor", "model"},
		{"repl", "--tree-key", "board", "--actor-id", " "},
		{"repl", "--tree-key", "board", "--max-nodes", "0"},
		{"repl", "--tree-key", "board", "--max-depth", "129"},
		{"repl", "--tree-key", "board", "--max-receipts", "10001"},
		{"repl", "--tree-key", "board", "--max-title-bytes", "not-an-int"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if code := runCLI(args, strings.NewReader(""), &output, &diagnostics); code != 2 || output.Len() != 0 || diagnostics.Len() == 0 {
				t.Fatalf("usage routing: exit=%d stdout=%q stderr=%q", code, &output, &diagnostics)
			}
			if code := runCLI(args, strings.NewReader(""), io.Discard, cliTestFailWriter{}); code != 1 {
				t.Fatalf("usage writer failure: exit=%d", code)
			}
		})
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}, {"run", "-h"}, {"run", "--help"}, {"inspect", "-h"}, {"inspect", "--help"}, {"repl", "-h"}, {"repl", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if code := runCLI(args, strings.NewReader(""), &output, &diagnostics); code != 0 || diagnostics.Len() != 0 || output.Len() == 0 {
				t.Fatalf("help routing: exit=%d stdout=%q stderr=%q", code, &output, &diagnostics)
			}
			if code := runCLI(args, strings.NewReader(""), io.Discard, cliTestFailWriter{}); code != 0 {
				t.Fatalf("successful help used diagnostics: exit=%d", code)
			}
			if code := runCLI(args, strings.NewReader(""), cliTestFailWriter{}, &diagnostics); code != 1 || diagnostics.Len() != 0 {
				t.Fatalf("help output failure routing: exit=%d stderr=%q", code, &diagnostics)
			}
		})
	}
}

func TestCLIValidationExpectationAndIOExitsPreserveCheckpoint(t *testing.T) {
	dir := t.TempDir()
	scenarioPath := filepath.Join(dir, "scenario.json")
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	init := cliTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`)
	cliTestJSONFile(t, scenarioPath, cliTestScenario(init))
	cliTestRun(t, []string{"run", "--snapshot", checkpointPath, scenarioPath}, "", 0)
	original, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	assertPreserved := func() {
		t.Helper()
		data, err := os.ReadFile(checkpointPath)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatal("failed command overwrote checkpoint")
		}
	}
	badExpectation := init
	badExpectation.Expect.Revision = cliTestPtr(tasktree.Revision(99))
	cliTestJSONFile(t, scenarioPath, cliTestScenario(badExpectation))
	cliTestRun(t, []string{"run", "--snapshot", checkpointPath, scenarioPath}, "", 1)
	assertPreserved()
	if err := os.WriteFile(scenarioPath, []byte(`{"schema_version":1,"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cliTestRun(t, []string{"run", "--snapshot", checkpointPath, scenarioPath}, "", 1)
	assertPreserved()
	mismatch := cliTestScenario()
	mismatch.TreeKey = "other"
	cliTestJSONFile(t, scenarioPath, mismatch)
	cliTestRun(t, []string{"run", "--load", checkpointPath, "--snapshot", checkpointPath, scenarioPath}, "", 1)
	assertPreserved()
	mismatch = cliTestScenario()
	mismatch.Limits.MaxReceipts++
	cliTestJSONFile(t, scenarioPath, mismatch)
	cliTestRun(t, []string{"run", "--load", checkpointPath, scenarioPath}, "", 1)
	corruptPath := filepath.Join(dir, "corrupt.json")
	corrupt := cliTestCheckpoint(t, checkpointPath)
	corrupt.Envelope.Snapshot.NextID = 1
	cliTestJSONFile(t, corruptPath, corrupt)
	cliTestRun(t, []string{"inspect", corruptPath}, "", 1)
	cliTestRun(t, []string{"run", "--load", corruptPath, scenarioPath}, "", 1)
	cliTestRun(t, []string{"run", filepath.Join(dir, "missing.json")}, "", 1)
	cliTestRun(t, []string{"inspect", filepath.Join(dir, "missing.json")}, "", 1)
	cliTestJSONFile(t, scenarioPath, cliTestScenario(init))
	cliTestRun(t, []string{"run", "--snapshot", dir, scenarioPath}, "", 1)
	cliTestRun(t, []string{"run", "--snapshot", filepath.Join(dir, "missing", "checkpoint.json"), scenarioPath}, "", 1)
	var diagnostics bytes.Buffer
	if code := runCLI([]string{"run", "--snapshot", checkpointPath, scenarioPath}, strings.NewReader(""), cliTestFailWriter{}, &diagnostics); code != 1 {
		t.Fatalf("writer failure exit=%d", code)
	}
	assertPreserved()
	if code := runCLI([]string{"inspect", checkpointPath}, strings.NewReader(""), cliTestFailWriter{}, &diagnostics); code != 1 {
		t.Fatalf("inspect writer failure exit=%d", code)
	}
	if code := runCLI([]string{"help"}, strings.NewReader(""), cliTestFailWriter{}, &diagnostics); code != 1 {
		t.Fatalf("help writer failure exit=%d", code)
	}
	if code := runCLI([]string{"repl", "--help"}, strings.NewReader(""), io.Discard, cliTestFailWriter{}); code != 0 {
		t.Fatalf("successful help used diagnostics: exit=%d", code)
	}
}

type cliTestFailWriter struct{}

func (cliTestFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type cliTestFailReader struct{}

func (cliTestFailReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func cliTestREPLArgs(snapshot string) []string {
	return []string{"repl", "--tree-key", "board", "--actor", "agent", "--actor-id", "agent-a", "--max-nodes", "50", "--max-depth", "8", "--max-title-bytes", "100", "--max-reason-bytes", "100", "--max-tombstones", "50", "--max-receipts", "50", "--snapshot", snapshot}
}

func cliTestLine(id, payload string) string {
	return `{"request_id":"` + id + `","payload":` + payload + "}\n"
}

func cliTestResults(t *testing.T, data []byte) []protocol.ToolResult {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
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

func TestCLIReplFixedAuthorityLimitsAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	args := cliTestREPLArgs(path)
	init := `{"op":"init","expected_revision":0,"items":["A"]}`
	input := cliTestLine("init", init) + cliTestLine("forbidden", `{"op":"rm","id":"n1","expected_revision":1}`) + cliTestLine("done", `{"op":"done","id":"n1","expected_revision":1}`)
	results := cliTestResults(t, cliTestRun(t, args, input, 0))
	checkpoint := cliTestCheckpoint(t, path)
	if len(results) != 3 || results[0].IsError || !results[1].IsError || results[1].Details.Problem == nil || results[2].IsError || checkpoint.Envelope.Revision != 2 || len(checkpoint.Envelope.Receipts) != 2 || checkpoint.Envelope.Snapshot.Nodes["n1"].Status != tasktree.Completed || checkpoint.Envelope.Receipts["done"].Actor != (tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}) {
		t.Fatal("REPL authority/error continuation/commit boundary failed")
	}
	if _, exists := checkpoint.Envelope.Receipts["forbidden"]; exists {
		t.Fatal("agent removal acquired receipt")
	}
	operatorArgs := append(cliTestREPLArgs(path), "--actor", "operator", "--actor-id", "operator-a", "--load", path)
	results = cliTestResults(t, cliTestRun(t, operatorArgs, cliTestLine("reopen", `{"op":"reopen","id":"n1","expected_revision":2}`), 0))
	checkpoint = cliTestCheckpoint(t, path)
	if len(results) != 1 || results[0].IsError || results[0].Details.Summary.ActiveID != "" || results[0].Details.Summary.NextID != "n1" || checkpoint.Envelope.Revision != 3 || checkpoint.Envelope.Snapshot.Nodes["n1"].Status != tasktree.Pending || checkpoint.Envelope.Receipts["reopen"].Actor != (tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator-a"}) {
		t.Fatal("operator reopen or fixed actor binding failed")
	}
	results = cliTestResults(t, cliTestRun(t, append(cliTestREPLArgs(path), "--load", path), cliTestLine("init", init)+cliTestLine("add", `{"op":"add","items":["B"],"expected_revision":3}`), 0))
	checkpoint = cliTestCheckpoint(t, path)
	if len(results) != 2 || !results[0].Details.Replayed || results[0].Details.Receipt == nil || results[0].Details.Receipt.CommittedRevision != 1 || results[0].Details.Summary.Revision != 3 || results[1].IsError || checkpoint.Envelope.Revision != 4 || checkpoint.Envelope.Snapshot.NextID != 3 || checkpoint.Envelope.Snapshot.Nodes["n2"].Title != "B" || len(checkpoint.Envelope.Receipts) != 4 {
		t.Fatal("CLI REPL load bypassed durable replay/counter semantics")
	}
	cliTestRun(t, []string{"repl", "--tree-key", "board", "--load", path}, "", 1)
	limited := append(cliTestREPLArgs(path), "--max-nodes", "2", "--max-receipts", "1")
	results = cliTestResults(t, cliTestRun(t, limited, cliTestLine("init", init)+cliTestLine("add", `{"op":"add","items":["B"],"expected_revision":1}`)+cliTestLine("done", `{"op":"done","id":"n1","expected_revision":1}`)+cliTestLine("init", init), 0))
	checkpoint = cliTestCheckpoint(t, path)
	if len(results) != 4 || results[0].IsError || !results[1].IsError || !results[2].IsError || !results[3].Details.Replayed || checkpoint.Limits.MaxNodes != 2 || checkpoint.Limits.MaxReceipts != 1 || checkpoint.Envelope.Revision != 1 || len(checkpoint.Envelope.Receipts) != 1 || checkpoint.Envelope.Snapshot.NextID != 2 || checkpoint.Envelope.Snapshot.Nodes["n1"].Status != tasktree.InProgress {
		t.Fatal("lab limit flags not enforced atomically or replay unavailable at capacity")
	}
	for _, index := range []int{1, 2} {
		if results[index].Details.Problem == nil || results[index].Details.Problem.Code != tasktree.CodeLimitExceeded || results[index].Details.Summary.Revision != 1 {
			t.Fatal("limit rejection failed structured error/current revision")
		}
	}
}

func TestCLIReplMalformedStreamAndTerminationExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	args := cliTestREPLArgs(path)
	init := cliTestLine("init", `{"op":"init","items":["A"],"expected_revision":0}`)
	cliTestRun(t, args, init+cliTestLine("done", `{"op":"done","id":"n1","expected_revision":1}`), 0)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"not json", `{"request_id":"r","payload":{"op":"view"},"tree_key":"other"}`, `{"request_id":"r","payload":{"op":"view"},"actor":"operator"}`} {
		output := cliTestRun(t, args, init+bad+"\n"+cliTestLine("done", `{"op":"done","id":"n1","expected_revision":1}`), 1)
		results := cliTestResults(t, output)
		if len(results) != 1 || results[0].IsError || results[0].Details.Receipt == nil || results[0].Details.Receipt.CommittedRevision != 1 {
			t.Fatal("malformed stream continued to execute commands")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("failed REPL overwrote checkpoint")
		}
	}
	for _, input := range []string{"", "\n" + init} {
		output := cliTestRun(t, args, input, 0)
		checkpoint := cliTestCheckpoint(t, path)
		if len(output) != 0 || checkpoint.Envelope.Revision != 0 || checkpoint.Envelope.Snapshot.Initialized || len(checkpoint.Envelope.Receipts) != 0 {
			t.Fatal("EOF/empty line failed clean termination")
		}
	}
	var diagnostics bytes.Buffer
	if code := runCLI(args, cliTestFailReader{}, io.Discard, &diagnostics); code != 1 {
		t.Fatalf("reader error exit=%d", code)
	}
	if code := runCLI(args, strings.NewReader(init), cliTestFailWriter{}, &diagnostics); code != 1 {
		t.Fatalf("writer error exit=%d", code)
	}
	checkpoint := cliTestCheckpoint(t, path)
	if checkpoint.Envelope.Revision != 0 || checkpoint.Envelope.Snapshot.Initialized || len(checkpoint.Envelope.Receipts) != 0 {
		t.Fatal("stream I/O failure published an export")
	}
	defaultPath := filepath.Join(dir, "defaults.json")
	cliTestRun(t, []string{"repl", "--tree-key", "defaults", "--snapshot", defaultPath}, init, 0)
	defaults := cliTestCheckpoint(t, defaultPath)
	receipt, exists := defaults.Envelope.Receipts["init"]
	if defaults.TreeKey != "defaults" || !exists || receipt.Actor.Kind != tasktree.ActorAgent || strings.TrimSpace(receipt.Actor.ID) == "" {
		t.Fatal("CLI default authority or tree binding invalid")
	}
	if err := tasktree.ValidateLimits(defaults.Limits); err != nil {
		t.Fatalf("CLI default limits invalid: %v", err)
	}
}
