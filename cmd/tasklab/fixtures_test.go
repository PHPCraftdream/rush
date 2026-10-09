package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
)

func cliFixturePath(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "internal", "tasktree", "lab", "testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIFixturePaths(t *testing.T) {
	for _, name := range []string{"basic", "blocked-only", "nested", "retries", "operator-delete", "boundaries"} {
		t.Run(name, func(t *testing.T) {
			path := cliFixturePath(t, name)
			scenario, err := loadScenario(path)
			if err != nil {
				t.Fatal(err)
			}
			report := cliTestDecode[lab.Report](t, cliTestRun(t, []string{"run", path}, "", 0))
			if len(report.Steps) != len(scenario.Steps) {
				t.Fatal("CLI skipped fixture expectations")
			}
			if name == "boundaries" && (report.Checkpoint.Envelope.Revision != 6 || len(report.Checkpoint.Envelope.Receipts) != 6) {
				t.Fatal("expected errors changed receipt history")
			}
		})
	}
}

func TestCLIFixtureExportInspectResume(t *testing.T) {
	checkpointPath := filepath.Join(t.TempDir(), "board.json")
	saved := cliTestDecode[lab.Report](t, cliTestRun(t, []string{"run", "--snapshot", checkpointPath, cliFixturePath(t, "export-save")}, "", 0))
	checkpoint := cliTestCheckpoint(t, checkpointPath)
	if !reflect.DeepEqual(checkpoint, saved.Checkpoint) {
		t.Fatal("snapshot differs from run report")
	}
	e := checkpoint.Envelope
	if e.Revision != 4 || e.Snapshot.NextID != 7 || len(e.Receipts) != 4 || !reflect.DeepEqual(e.Snapshot.Nodes["n2"].Children, []tasktree.NodeID{"n4", "n3"}) || e.Snapshot.Nodes["n3"].Reason != "waiting" || e.Snapshot.Nodes["n6"].Status != tasktree.InProgress {
		t.Fatal("file export lost nested order/blocker/focus/counter/receipts")
	}
	if !reflect.DeepEqual(e.Snapshot.TitleGuards, []tasktree.TitleGuard{{Kind: tasktree.KindTask, Title: "Gone"}}) || e.Snapshot.Tombstones["n5"].Actor.ID != "operator-a" {
		t.Fatal("file export lost removal authority/guard")
	}
	inspected := cliTestDecode[lab.Checkpoint](t, cliTestRun(t, []string{"inspect", checkpointPath}, "", 0))
	if !reflect.DeepEqual(inspected, checkpoint) {
		t.Fatal("inspect omitted retained envelope")
	}
	resumed := cliTestDecode[lab.Report](t, cliTestRun(t, []string{"run", "--load", checkpointPath, "--snapshot", checkpointPath, cliFixturePath(t, "export-resume")}, "", 0))
	if resumed.Checkpoint.Envelope.Revision != 6 || resumed.Checkpoint.Envelope.Snapshot.NextID != 9 || len(resumed.Checkpoint.Envelope.Receipts) != 6 || len(resumed.Checkpoint.Envelope.Snapshot.TitleGuards) != 0 {
		t.Fatal("CLI restart lost receipt/counter/guard continuation")
	}
	for id, receipt := range checkpoint.Envelope.Receipts {
		if !reflect.DeepEqual(resumed.Checkpoint.Envelope.Receipts[id], receipt) {
			t.Fatalf("CLI restart changed old receipt %s", id)
		}
	}
	first := resumed.Steps[0].Result
	if !first.Details.Replayed || first.Details.Receipt == nil || first.Details.Receipt.CommittedRevision != 1 || first.Details.Summary.Revision != 4 || len(first.Details.Created) != 6 || !first.Details.Created[4].Removed {
		t.Fatal("CLI retry did not expose original receipt with current removed brief")
	}
	if !resumed.Steps[1].Result.IsError || resumed.Steps[1].Result.Details.Problem == nil || resumed.Steps[1].Result.Details.Problem.Code != tasktree.CodeRemovedByOperator {
		t.Fatal("expected guard error lost across restart")
	}
	if !reflect.DeepEqual(cliTestCheckpoint(t, checkpointPath), resumed.Checkpoint) {
		t.Fatal("resumed checkpoint not persisted")
	}
}

func TestCLIFixtureTamperedExpectationDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "retained.json")
	cliTestRun(t, []string{"run", "--snapshot", checkpointPath, cliFixturePath(t, "export-save")}, "", 0)
	before, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := loadScenario(cliFixturePath(t, "export-resume"))
	if err != nil {
		t.Fatal(err)
	}
	*scenario.Steps[0].Expect.ReceiptRevision = 4
	path := filepath.Join(dir, "tampered.json")
	cliTestJSONFile(t, path, scenario)
	cliTestRun(t, []string{"run", "--load", checkpointPath, "--snapshot", checkpointPath, path}, "", 1)
	after, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed fixture overwrote loaded checkpoint")
	}
}
