package memory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func serviceTestImportedSeed(t *testing.T) tt.Envelope {
	t.Helper()
	seed := serviceTestCandidate(t)
	seed.Revision = 50
	seed.Receipts = nil
	seed.Snapshot.NextID = 5
	seed.Snapshot.Tombstones = map[tt.NodeID]tt.Tombstone{"n2": {ID: "n2", Kind: tt.KindTask, Title: "removed", Actor: tt.Actor{Kind: tt.ActorOperator, ID: "operator"}}}
	seed.Snapshot.TitleGuards = []tt.TitleGuard{{Kind: tt.KindTask, Title: "removed"}}
	seed.Receipts = map[tt.RequestID]tt.Receipt{
		"retained": {RequestID: "retained", Actor: tt.Actor{Kind: tt.ActorAgent, ID: "host"}, Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CommittedRevision: 7},
	}
	if err := tt.CheckEnvelope(seed, serviceTestLimits()); err != nil {
		t.Fatal(err)
	}
	return seed
}

func serviceTestImportedCandidate(seed tt.Envelope) tt.Envelope {
	candidate := tt.CloneEnvelope(seed)
	candidate.Revision = seed.Revision + 1
	candidate.Receipts["new"] = tt.Receipt{RequestID: "new", Actor: tt.Actor{Kind: tt.ActorAgent, ID: "host"}, Fingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CommittedRevision: candidate.Revision}
	return candidate
}

func TestMemoryImportedHistoryConservation(t *testing.T) {
	seed := serviceTestImportedSeed(t)
	cases := map[string]func(*tt.Envelope){
		"receipt eviction":   func(e *tt.Envelope) { delete(e.Receipts, "retained") },
		"receipt rewrite":    func(e *tt.Envelope) { r := e.Receipts["retained"]; r.Actor.ID = "changed"; e.Receipts["retained"] = r },
		"tombstone eviction": func(e *tt.Envelope) { delete(e.Snapshot.Tombstones, "n2") },
		"tombstone rewrite": func(e *tt.Envelope) {
			r := e.Snapshot.Tombstones["n2"]
			r.Actor.ID = "changed/operator"
			e.Snapshot.Tombstones["n2"] = r
		},
		"new receipt old revision": func(e *tt.Envelope) { r := e.Receipts["new"]; r.CommittedRevision = 30; e.Receipts["new"] = r },
		"silent live disappearance": func(e *tt.Envelope) {
			delete(e.Snapshot.Nodes, "n1")
			root := e.Snapshot.Nodes[tt.RootID]
			root.Children = nil
			e.Snapshot.Nodes[tt.RootID] = root
		},
		"live kind change": func(e *tt.Envelope) {
			n := e.Snapshot.Nodes["n1"]
			n.Kind = tt.KindGroup
			n.Status = ""
			n.ActiveForm = ""
			n.Reason = ""
			e.Snapshot.Nodes["n1"] = n
		},
		"counter rewind":        func(e *tt.Envelope) { e.Snapshot.NextID = 3 },
		"initialization rewind": func(e *tt.Envelope) { e.Snapshot.Initialized = false },
		"zero new receipts":     func(e *tt.Envelope) { delete(e.Receipts, "new") },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": seed})
			if err != nil {
				t.Fatal(err)
			}
			before := serviceTestLoad(t, s)
			candidate := serviceTestImportedCandidate(seed)
			corrupt(&candidate)
			if name != "initialization rewind" {
				if err := tt.CheckEnvelope(candidate, serviceTestLimits()); err != nil {
					t.Fatalf("candidate should be structurally valid before conservation check: %v", err)
				}
			}
			if err := s.Commit(context.Background(), "board", seed.Revision, candidate); err == nil {
				t.Fatal("history/identity violation accepted")
			}
			if !reflect.DeepEqual(before, serviceTestLoad(t, s)) {
				t.Fatal("rejected candidate changed publication")
			}
		})
	}
	s, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": seed})
	if err != nil {
		t.Fatal(err)
	}
	candidate := serviceTestImportedCandidate(seed)
	if err := s.Commit(context.Background(), "board", seed.Revision, candidate); err != nil {
		t.Fatal("valid gapped append rejected:", err)
	}
	if !reflect.DeepEqual(candidate, serviceTestLoad(t, s)) {
		t.Fatal("valid append lost imported history")
	}
}

func TestMemoryTwoNewReceiptsRejectDuplicateCandidateRevision(t *testing.T) {
	seed := serviceTestImportedSeed(t)
	store, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": seed})
	if err != nil {
		t.Fatal(err)
	}
	before := serviceTestLoad(t, store)
	candidate := serviceTestImportedCandidate(seed)
	extra := candidate.Receipts["new"]
	extra.RequestID = "extra"
	candidate.Receipts["extra"] = extra
	var problem *tt.Problem
	if err := tt.CheckEnvelope(candidate, serviceTestLimits()); !errors.As(err, &problem) || problem.Code != tt.CodeInvalidSnapshot {
		t.Fatalf("duplicate candidate receipt revisions not rejected as invalid snapshot: %v", err)
	}
	problem = nil
	if err := store.Commit(context.Background(), "board", seed.Revision, candidate); !errors.As(err, &problem) || problem.Code != tt.CodeInvalidSnapshot {
		t.Fatalf("two-new-receipt candidate not rejected as invalid snapshot: %v", err)
	}
	if !reflect.DeepEqual(before, serviceTestLoad(t, store)) {
		t.Fatal("rejected two-new-receipt candidate changed publication")
	}
}
