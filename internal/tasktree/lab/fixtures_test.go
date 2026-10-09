package lab

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func fixtureScenario(t *testing.T, name string) Scenario {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scenario, err := ReadScenario(file)
	if err != nil {
		t.Fatal(err)
	}
	return scenario
}

func TestFixtures(t *testing.T) {
	for _, name := range []string{"basic", "blocked-only", "nested", "retries", "operator-delete", "boundaries", "export-save"} {
		t.Run(name, func(t *testing.T) {
			scenario := fixtureScenario(t, name)
			report, err := Run(context.Background(), scenario, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Steps) != len(scenario.Steps) {
				t.Fatal("lost fixture steps")
			}
			for i, step := range scenario.Steps {
				result := report.Steps[i].Result
				_, retained := report.Checkpoint.Envelope.Receipts[step.RequestID]
				if result.IsError && step.Mode == "" && retained {
					t.Fatalf("failed request %s retained receipt", step.Label)
				}
				if !result.IsError && result.Details.Receipt != nil {
					if !reflect.DeepEqual(report.Checkpoint.Envelope.Receipts[step.RequestID], *result.Details.Receipt) {
						t.Fatalf("receipt changed for %s", step.Label)
					}
				}
			}
			switch name {
			case "retries":
				if report.Checkpoint.Envelope.Revision != 3 || len(report.Checkpoint.Envelope.Receipts) != 3 {
					t.Fatal("reuse/conflict/replay committed or no-op did not commit")
				}
				delta := report.Checkpoint.Envelope.Receipts["noop"].Delta
				if len(delta.Created)+len(delta.Updated)+len(delta.Removed)+len(delta.Completed) != 0 {
					t.Fatal("semantic no-op emitted transitions")
				}
			case "boundaries":
				e := report.Checkpoint.Envelope
				if e.Revision != 6 || len(e.Receipts) != 6 || e.Snapshot.NextID != 5 || len(e.Snapshot.Tombstones) != 1 || !reflect.DeepEqual(e.Snapshot.TitleGuards, []tasktree.TitleGuard{{Kind: tasktree.KindTask, Title: "BBBB"}}) {
					t.Fatal("boundary failure partially published state/history/counter")
				}
				created := report.Steps[len(report.Steps)-1].Result.Details.Created
				if len(created) != 3 || created[2].ID != "n3" || !created[2].Removed {
					t.Fatal("old receipt recreated removed boundary node")
				}
			case "operator-delete":
				if len(report.Checkpoint.Envelope.Snapshot.TitleGuards) != 0 || report.Checkpoint.Envelope.Snapshot.NextID != 6 {
					t.Fatal("operator readd failed to clear guard or reused id")
				}
			case "blocked-only":
				fixtureScopedProgress(t, scenario)
			}
		})
	}
	t.Run("export-resume", func(t *testing.T) {
		saved, err := Run(context.Background(), fixtureScenario(t, "export-save"), nil)
		if err != nil {
			t.Fatal(err)
		}
		fixtureSavedEnvelope(t, saved.Checkpoint)
		var wire bytes.Buffer
		if err := WriteCheckpoint(&wire, saved.Checkpoint); err != nil {
			t.Fatal(err)
		}
		loaded, err := ReadCheckpoint(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(saved.Checkpoint, loaded) {
			t.Fatal("codec lost retained envelope")
		}
		resumed, err := Run(context.Background(), fixtureScenario(t, "export-resume"), &loaded)
		if err != nil {
			t.Fatal(err)
		}
		e := resumed.Checkpoint.Envelope
		if e.Revision != 6 || e.Snapshot.NextID != 9 || len(e.Receipts) != 6 || len(e.Snapshot.TitleGuards) != 0 {
			t.Fatal("resume lost history or id continuity")
		}
		for id, receipt := range loaded.Envelope.Receipts {
			if !reflect.DeepEqual(e.Receipts[id], receipt) {
				t.Fatalf("resume altered original receipt %s", id)
			}
		}
		briefs := resumed.Steps[0].Result.Details.Created
		if len(briefs) != 6 || briefs[4].ID != "n5" || !briefs[4].Removed || briefs[2].Status != tasktree.Blocked || briefs[2].Reason != "waiting" {
			t.Fatal("replay returned historical instead of current briefs")
		}
	})
}

func fixtureSavedEnvelope(t *testing.T, checkpoint Checkpoint) {
	t.Helper()
	e := checkpoint.Envelope
	s := e.Snapshot
	if checkpoint.TreeKey != "export" || e.Revision != 4 || len(e.Receipts) != 4 || s.NextID != 7 || !s.Initialized {
		t.Fatal("save lost binding/revision/history/counter")
	}
	if !reflect.DeepEqual(s.Nodes["n0"].Children, []tasktree.NodeID{"n1", "n6"}) || !reflect.DeepEqual(s.Nodes["n1"].Children, []tasktree.NodeID{"n2"}) || !reflect.DeepEqual(s.Nodes["n2"].Children, []tasktree.NodeID{"n4", "n3"}) {
		t.Fatal("save lost nested ordering")
	}
	if s.Nodes["n3"].Reason != "waiting" || s.Nodes["n4"].Status != tasktree.Blocked || s.Nodes["n6"].Status != tasktree.InProgress {
		t.Fatal("save lost blockers/focus")
	}
	if s.Tombstones["n5"].Actor != (tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator-a"}) || !reflect.DeepEqual(s.TitleGuards, []tasktree.TitleGuard{{Kind: tasktree.KindTask, Title: "Gone"}}) {
		t.Fatal("save lost trusted removal/guard")
	}
	if e.Receipts["init"].CommittedRevision != 1 || e.Receipts["init"].Actor != (tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}) || e.Receipts["init"].Fingerprint == "" || !reflect.DeepEqual(e.Receipts["init"].Delta.Created, []tasktree.NodeID{"n1", "n2", "n3", "n4", "n5", "n6"}) {
		t.Fatal("save lost canonical original receipt")
	}
}

func fixtureScopedProgress(t *testing.T, scenario Scenario) {
	t.Helper()
	scenario.Steps = scenario.Steps[:5]
	scenario.Steps = append(scenario.Steps,
		Step{Label: "empty scope", RequestID: "empty-scope", Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}, Payload: []byte(`{"op":"view","id":"n5"}`), Expect: scenario.Steps[4].Expect},
		Step{Label: "abandoned scope", RequestID: "abandoned-scope", Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent-a"}, Payload: []byte(`{"op":"view","id":"n1"}`), Expect: scenario.Steps[4].Expect},
	)
	scenario.Steps[5].Expect.ReceiptRevision = nil
	scenario.Steps[6].Expect.ReceiptRevision = nil
	report, err := Run(context.Background(), scenario, nil)
	if err != nil {
		t.Fatal(err)
	}
	empty := report.Steps[5].Result.Details.View
	abandoned := report.Steps[6].Result.Details.View
	wantEmpty := tasktree.Progress{Total: 0, Pending: 0, InProgress: 0, Completed: 0, Blocked: 0, Abandoned: 0, Actionable: 0, Unfinished: 0, Settled: 0, AllSettled: false, AllCompleted: false}
	if empty == nil || empty.Root.ID != "n5" || empty.Root.Progress != wantEmpty {
		t.Fatal("empty group counted as successful work")
	}
	want := tasktree.Progress{Total: 2, Pending: 0, InProgress: 0, Completed: 0, Blocked: 0, Abandoned: 2, Actionable: 0, Unfinished: 0, Settled: 2, AllSettled: true, AllCompleted: false}
	if abandoned == nil || abandoned.Root.ID != "n1" || abandoned.Root.Progress != want {
		t.Fatal("all abandoned scope counted as completed")
	}
}

func TestFixtureOraclesRejectTampering(t *testing.T) {
	for _, field := range []string{"revision", "receipt", "focus", "progress", "order", "title", "absence"} {
		t.Run(field, func(t *testing.T) {
			s := fixtureScenario(t, "basic")
			s.Steps = s.Steps[:2]
			e := &s.Steps[1].Expect
			switch field {
			case "revision":
				*e.Revision = 2
			case "receipt":
				*e.ReceiptRevision = 2
			case "focus":
				*e.ActiveID = "n2"
			case "progress":
				e.Progress.AllCompleted = true
			case "order":
				*e.Nodes[0].Children = []tasktree.NodeID{"n3", "n2", "n1"}
			case "title":
				*e.Nodes[1].Title = "wrong"
			case "absence":
				e.AbsentIDs = []tasktree.NodeID{"n1"}
			}
			if _, err := Run(context.Background(), s, nil); err == nil {
				t.Fatal("accepted tampered explicit oracle")
			}
		})
	}
}
