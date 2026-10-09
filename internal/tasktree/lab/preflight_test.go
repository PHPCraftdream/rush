package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func TestWriteCheckpointExactEncodedBoundary(t *testing.T) {
	checkpoint := labTestRun(t, labTestScenario(), nil).Checkpoint
	baseline, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.TreeKey += tasktree.TreeKey(strings.Repeat("x", MaxInputBytes-len(baseline)-1))
	var out bytes.Buffer
	if err := WriteCheckpoint(&out, checkpoint); err != nil {
		t.Fatal(err)
	}
	if out.Len() != MaxInputBytes || out.Bytes()[out.Len()-1] != '\n' {
		t.Fatal("incorrect inclusive newline boundary")
	}
	got, err := ReadCheckpoint(bytes.NewReader(out.Bytes()))
	if err != nil || !reflect.DeepEqual(got, checkpoint) {
		t.Fatalf("boundary roundtrip: %v", err)
	}
	checkpoint.TreeKey += "x"
	out.Reset()
	if err := WriteCheckpoint(&out, checkpoint); err == nil || out.Len() != 0 {
		t.Fatal("oversized encoder chunk forwarded")
	}
}

type labTestFailureWriter struct{ err error }

func (w labTestFailureWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

func TestCheckpointBoundedWriterHonesty(t *testing.T) {
	failure := errors.New("writer failure")
	checkpoint := labTestRun(t, labTestScenario(), nil).Checkpoint
	if err := WriteCheckpoint(labTestFailureWriter{failure}, checkpoint); !errors.Is(err, failure) {
		t.Fatalf("lost wrapped writer failure: %v", err)
	}
	if err := WriteCheckpoint(labTestShortWriter{}, checkpoint); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("lost short write: %v", err)
	}
}

func TestScenarioTypedPreflightBeforeLoadedValidation(t *testing.T) {
	large := strings.Repeat("x", MaxInputBytes)
	cases := map[string]func(*Scenario){
		"header":     func(s *Scenario) { s.TreeKey = tasktree.TreeKey(large + "x") },
		"label":      func(s *Scenario) { s.Steps[0].Label = large },
		"actor id":   func(s *Scenario) { s.Steps[0].Actor.ID = large },
		"actor kind": func(s *Scenario) { s.Steps[0].Actor.Kind = tasktree.ActorKind(large) },
		"request":    func(s *Scenario) { s.Steps[0].RequestID = tasktree.RequestID(large) },
		"mode":       func(s *Scenario) { s.Steps[0].Mode = large },
		"payload":    func(s *Scenario) { s.Steps[0].Payload = json.RawMessage(large) },
		"problem":    func(s *Scenario) { s.Steps[0].Expect.ProblemCode = tasktree.ProblemCode(large) },
		"active":     func(s *Scenario) { s.Steps[0].Expect.ActiveID = labTestPtr(tasktree.NodeID(large)) },
		"next":       func(s *Scenario) { s.Steps[0].Expect.NextID = labTestPtr(tasktree.NodeID(large)) },
		"removed":    func(s *Scenario) { s.Steps[0].Expect.RemovedIDs = []tasktree.NodeID{tasktree.NodeID(large)} },
		"absent":     func(s *Scenario) { s.Steps[0].Expect.AbsentIDs = []tasktree.NodeID{tasktree.NodeID(large)} },
		"steps":      func(s *Scenario) { s.Steps = make([]Step, MaxScenarioSteps+1) },
		"expectation collection": func(s *Scenario) {
			chunk := large[:1024]
			s.Steps[0].Expect.Nodes = make([]NodeExpectation, MaxInputBytes/len(chunk))
			for i := range s.Steps[0].Expect.Nodes {
				s.Steps[0].Expect.Nodes[i].Title = &chunk
			}
		},
		"id collection": func(s *Scenario) {
			s.Steps[0].Expect.RemovedIDs = make([]tasktree.NodeID, MaxInputBytes/1024)
			for i := range s.Steps[0].Expect.RemovedIDs {
				s.Steps[0].Expect.RemovedIDs[i] = tasktree.NodeID(large[:1024])
			}
		},
		"aggregate": func(s *Scenario) {
			half := large[:MaxInputBytes/2]
			s.Steps[0].Label = half
			s.Steps[0].Actor.ID = half
		},
	}
	for _, field := range []string{"id", "parent", "kind", "title", "status", "reason", "form", "children"} {
		cases["node "+field] = func(s *Scenario) {
			n := NodeExpectation{}
			switch field {
			case "id":
				n.ID = tasktree.NodeID(large)
			case "parent":
				n.ParentID = labTestPtr(tasktree.NodeID(large))
			case "kind":
				n.Kind = labTestPtr(tasktree.NodeKind(large))
			case "title":
				n.Title = &large
			case "status":
				n.Status = labTestPtr(tasktree.Status(large))
			case "reason":
				n.Reason = &large
			case "form":
				n.ActiveForm = &large
			case "children":
				n.Children = labTestPtr([]tasktree.NodeID{tasktree.NodeID(large)})
			}
			s.Steps[0].Expect.Nodes = []NodeExpectation{n}
		}
	}
	checkpoint := labTestRun(t, labTestScenario(labTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["A"]}`)), nil).Checkpoint
	viewScenario := func() Scenario {
		step := labTestStep("view", "read", `{"op":"view"}`)
		step.Expect.Revision = labTestPtr(tasktree.Revision(1))
		step.Expect.Progress = &tasktree.Progress{
			Total: 1, Pending: 0, InProgress: 1, Completed: 0, Blocked: 0,
			Abandoned: 0, Actionable: 1, Unfinished: 1, Settled: 0,
			AllSettled: false, AllCompleted: false,
		}
		return labTestScenario(step)
	}
	// Verify the unmodified view and initialized checkpoint are valid together.
	labTestRun(t, viewScenario(), &checkpoint)
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			for _, invalidLoaded := range []bool{false, true} {
				loadedName := "valid loaded"
				if invalidLoaded {
					loadedName = "invalid loaded ordering"
				}
				t.Run(loadedName, func(t *testing.T) {
					s := viewScenario()
					change(&s)
					loaded := checkpoint
					loaded.Envelope = tasktree.CloneEnvelope(checkpoint.Envelope)
					if invalidLoaded {
						loaded.SchemaVersion = 0 // Preflight must precede even loaded binding validation.
					}
					before := loaded
					before.Envelope = tasktree.CloneEnvelope(loaded.Envelope)
					report, err := Run(context.Background(), s, &loaded)
					if err == nil || !strings.Contains(err.Error(), "lab") || len(report.Steps) != 0 {
						t.Fatalf("preflight: %v, retained=%d", err, len(report.Steps))
					}
					if !reflect.DeepEqual(before, loaded) {
						t.Fatal("loaded checkpoint changed")
					}
				})
			}
		})
	}
}

func TestCheckpointTypedPreflightFields(t *testing.T) {
	large := strings.Repeat("x", MaxInputBytes)
	// Deliberately malformed DTOs ensure the LAB bound wins over envelope validation.
	cases := map[string]func(*Checkpoint){
		"key":          func(c *Checkpoint) { c.TreeKey = tasktree.TreeKey(large) },
		"root":         func(c *Checkpoint) { c.Envelope.Snapshot.RootID = tasktree.NodeID(large) },
		"node map key": func(c *Checkpoint) { c.Envelope.Snapshot.Nodes[tasktree.NodeID(large)] = tasktree.Node{} },
		"tombstone map key": func(c *Checkpoint) {
			c.Envelope.Snapshot.Tombstones = map[tasktree.NodeID]tasktree.Tombstone{tasktree.NodeID(large): {}}
		},
		"receipt map key": func(c *Checkpoint) {
			c.Envelope.Receipts = map[tasktree.RequestID]tasktree.Receipt{tasktree.RequestID(large): {}}
		},
	}
	for _, field := range []string{"id", "parent", "kind", "title", "form", "status", "reason", "children"} {
		cases["node "+field] = func(c *Checkpoint) {
			n := tasktree.Node{}
			switch field {
			case "id":
				n.ID = tasktree.NodeID(large)
			case "parent":
				n.ParentID = tasktree.NodeID(large)
			case "kind":
				n.Kind = tasktree.NodeKind(large)
			case "title":
				n.Title = large
			case "form":
				n.ActiveForm = large
			case "status":
				n.Status = tasktree.Status(large)
			case "reason":
				n.Reason = large
			case "children":
				n.Children = []tasktree.NodeID{tasktree.NodeID(large)}
			}
			c.Envelope.Snapshot.Nodes["n1"] = n
		}
	}
	for _, field := range []string{"id", "kind", "title", "actor kind", "actor id"} {
		cases["tombstone "+field] = func(c *Checkpoint) {
			n := tasktree.Tombstone{}
			switch field {
			case "id":
				n.ID = tasktree.NodeID(large)
			case "kind":
				n.Kind = tasktree.NodeKind(large)
			case "title":
				n.Title = large
			case "actor kind":
				n.Actor.Kind = tasktree.ActorKind(large)
			case "actor id":
				n.Actor.ID = large
			}
			c.Envelope.Snapshot.Tombstones = map[tasktree.NodeID]tasktree.Tombstone{"n1": n}
		}
	}
	for _, field := range []string{"kind", "title"} {
		cases["guard "+field] = func(c *Checkpoint) {
			g := tasktree.TitleGuard{}
			if field == "kind" {
				g.Kind = tasktree.NodeKind(large)
			} else {
				g.Title = large
			}
			c.Envelope.Snapshot.TitleGuards = []tasktree.TitleGuard{g}
		}
	}
	for _, field := range []string{"request", "fingerprint", "actor kind", "actor id", "created", "updated", "removed", "completed"} {
		cases["receipt "+field] = func(c *Checkpoint) {
			r := tasktree.Receipt{}
			ids := []tasktree.NodeID{tasktree.NodeID(large)}
			switch field {
			case "request":
				r.RequestID = tasktree.RequestID(large)
			case "fingerprint":
				r.Fingerprint = large
			case "actor kind":
				r.Actor.Kind = tasktree.ActorKind(large)
			case "actor id":
				r.Actor.ID = large
			case "created":
				r.Delta.Created = ids
			case "updated":
				r.Delta.Updated = ids
			case "removed":
				r.Delta.Removed = ids
			case "completed":
				r.Delta.Completed = ids
			}
			c.Envelope.Receipts = map[tasktree.RequestID]tasktree.Receipt{"r": r}
		}
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := labTestRun(t, labTestScenario(), nil).Checkpoint
			change(&c)
			before := tasktree.CloneEnvelope(c.Envelope)
			var out bytes.Buffer
			if err := WriteCheckpoint(&out, c); err == nil || out.Len() != 0 {
				t.Fatalf("preflight: %v", err)
			}
			s := labTestScenario(labTestStep("read", "read", `{"op":"view"}`))
			report, err := Run(context.Background(), s, &c)
			if err == nil || len(report.Steps) != 0 {
				t.Fatalf("loaded preflight: %v", err)
			}
			if !reflect.DeepEqual(before, c.Envelope) {
				t.Fatal("preflight changed envelope")
			}
		})
	}
}

func TestPreflightCollectionAccountingAndOverflow(t *testing.T) {
	for _, kind := range []string{"nodes", "tombstones", "receipts", "delta"} {
		t.Run(kind, func(t *testing.T) {
			c := labTestRun(t, labTestScenario(), nil).Checkpoint
			chunk := strings.Repeat("x", 1024)
			if kind == "delta" {
				ids := make([]tasktree.NodeID, MaxInputBytes/len(chunk))
				for i := range ids {
					ids[i] = tasktree.NodeID(chunk)
				}
				c.Envelope.Receipts = map[tasktree.RequestID]tasktree.Receipt{"r": {Delta: tasktree.Delta{Created: ids}}}
			} else {
				c.Envelope.Snapshot.Nodes = make(map[tasktree.NodeID]tasktree.Node)
				c.Envelope.Snapshot.Tombstones = make(map[tasktree.NodeID]tasktree.Tombstone)
				c.Envelope.Receipts = make(map[tasktree.RequestID]tasktree.Receipt)
				for i := 0; i < MaxInputBytes/len(chunk); i++ {
					key := strconv.Itoa(i)
					switch kind {
					case "nodes":
						c.Envelope.Snapshot.Nodes[tasktree.NodeID(key)] = tasktree.Node{Title: chunk}
					case "tombstones":
						c.Envelope.Snapshot.Tombstones[tasktree.NodeID(key)] = tasktree.Tombstone{Title: chunk}
					case "receipts":
						c.Envelope.Receipts[tasktree.RequestID(key)] = tasktree.Receipt{Fingerprint: chunk}
					}
				}
			}
			before := tasktree.CloneEnvelope(c.Envelope)
			var out bytes.Buffer
			if err := WriteCheckpoint(&out, c); err == nil || out.Len() != 0 {
				t.Fatalf("collection preflight: %v", err)
			}
			report, err := Run(context.Background(), labTestScenario(), &c)
			if err == nil || len(report.Steps) != 0 || !reflect.DeepEqual(before, c.Envelope) {
				t.Fatalf("loaded collection preflight: %v", err)
			}
		})
	}
	c := labTestRun(t, labTestScenario(), nil).Checkpoint
	// Shared strings keep generated memory bounded while aggregate bytes exceed 4 MiB.
	chunk := strings.Repeat("x", 1024)
	c.Envelope.Snapshot.TitleGuards = make([]tasktree.TitleGuard, MaxInputBytes/len(chunk))
	for i := range c.Envelope.Snapshot.TitleGuards {
		c.Envelope.Snapshot.TitleGuards[i].Title = chunk
	}
	var out bytes.Buffer
	if err := WriteCheckpoint(&out, c); err == nil || out.Len() != 0 {
		t.Fatalf("aggregate collection: %v", err)
	}
}
