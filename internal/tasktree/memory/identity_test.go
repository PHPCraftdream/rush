package memory_test

import (
	"context"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func TestMemoryInitializationCannotRewind(t *testing.T) {
	seed := tt.Envelope{Revision: 8, Snapshot: tt.EmptySnapshot(), Receipts: map[tt.RequestID]tt.Receipt{}}
	seed.Snapshot.Initialized = true
	store, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": seed})
	if err != nil {
		t.Fatal(err)
	}
	candidate := serviceTestImportedCandidate(seed)
	candidate.Snapshot.Initialized = false
	if err := tt.CheckEnvelope(candidate, serviceTestLimits()); err != nil {
		t.Fatal("uninitialized positive baseline must be structurally valid:", err)
	}
	if err := store.Commit(context.Background(), "board", 8, candidate); err == nil {
		t.Fatal("initialization rewound")
	}
	if !reflect.DeepEqual(seed, serviceTestLoad(t, store)) {
		t.Fatal("rejected rewind changed state")
	}
}

func TestMemoryExistingLiveIdentityMayBecomeTombstone(t *testing.T) {
	seed := serviceTestImportedSeed(t)
	store, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": seed})
	if err != nil {
		t.Fatal(err)
	}
	candidate := serviceTestImportedCandidate(seed)
	n := candidate.Snapshot.Nodes["n1"]
	delete(candidate.Snapshot.Nodes, "n1")
	root := candidate.Snapshot.Nodes[tt.RootID]
	root.Children = nil
	candidate.Snapshot.Nodes[tt.RootID] = root
	actor := tt.Actor{Kind: tt.ActorOperator, ID: "operator"}
	candidate.Snapshot.Tombstones["n1"] = tt.Tombstone{ID: "n1", Kind: n.Kind, Title: n.Title, Actor: actor}
	candidate.Snapshot.TitleGuards = append(candidate.Snapshot.TitleGuards, tt.TitleGuard{Kind: n.Kind, Title: n.Title})
	r := candidate.Receipts["new"]
	r.Actor = actor
	r.Delta.Removed = []tt.NodeID{"n1"}
	candidate.Receipts["new"] = r
	if err := store.Commit(context.Background(), "board", seed.Revision, candidate); err != nil {
		t.Fatal("same-kind tombstone rejected:", err)
	}
	if !reflect.DeepEqual(candidate, serviceTestLoad(t, store)) {
		t.Fatal("removal failed to conserve history")
	}
}
