package memory_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func serviceTestLimits() tt.Limits {
	return tt.Limits{MaxNodes: 20, MaxDepth: 5, MaxTitleBytes: 100, MaxReasonBytes: 100, MaxTombstones: 20, MaxReceipts: 20}
}
func serviceTestStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.NewStore(serviceTestLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func serviceTestCandidate(t *testing.T) tt.Envelope {
	t.Helper()
	s := serviceTestStore(t)
	_, err := tt.NewService(s, serviceTestLimits()).Mutate(context.Background(), "board", tt.Actor{Kind: tt.ActorAgent, ID: "host"}, "init", 0, tt.Command{Op: tt.OpInit, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "A"}}})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Load(context.Background(), "board")
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func serviceTestLoad(t *testing.T, s *memory.Store) tt.Envelope {
	t.Helper()
	e, err := s.Load(context.Background(), "board")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMemoryDetachedSeedsLoadsAndCommits(t *testing.T) {
	candidate := serviceTestCandidate(t)
	for _, mode := range []string{"seed", "commit"} {
		t.Run(mode, func(t *testing.T) {
			input := tt.CloneEnvelope(candidate)
			var s *memory.Store
			var err error
			if mode == "seed" {
				s, err = memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": input})
			} else {
				s = serviceTestStore(t)
				err = s.Commit(context.Background(), "board", 0, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			n := input.Snapshot.Nodes["n0"]
			n.Children[0] = "nzzz"
			r := input.Receipts["init"]
			r.Delta.Created[0] = "nzzz"
			delete(input.Snapshot.Nodes, "n1")
			delete(input.Receipts, "init")
			loaded := serviceTestLoad(t, s)
			if !reflect.DeepEqual(candidate, loaded) {
				t.Fatal("input aliases publication")
			}
			n = loaded.Snapshot.Nodes["n0"]
			n.Children[0] = "nzzz"
			r = loaded.Receipts["init"]
			r.Delta.Created[0] = "nzzz"
			delete(loaded.Snapshot.Nodes, "n1")
			delete(loaded.Receipts, "init")
			if !reflect.DeepEqual(candidate, serviceTestLoad(t, s)) {
				t.Fatal("load aliases publication")
			}
		})
	}
}

func TestMemoryAtomicCAS(t *testing.T) {
	s := serviceTestStore(t)
	candidate := serviceTestCandidate(t)
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { ready <- struct{}{}; <-start; results <- s.Commit(context.Background(), "board", 0, candidate) }()
	}
	<-ready
	<-ready
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("want one winner: %v %v", a, b)
	}
	loser := a
	if loser == nil {
		loser = b
	}
	var problem *tt.Problem
	if !errors.As(loser, &problem) || problem.Code != tt.CodeConflict {
		t.Fatalf("want conflict, got %v", loser)
	}
	if !reflect.DeepEqual(candidate, serviceTestLoad(t, s)) {
		t.Fatal("CAS did not publish full envelope")
	}
}

func TestMemoryInvalidCandidateAndCancellationLeaveState(t *testing.T) {
	candidate := serviceTestCandidate(t)
	for _, mode := range []string{"revision", "corrupt", "canceled", "history"} {
		t.Run(mode, func(t *testing.T) {
			s := serviceTestStore(t)
			before := serviceTestLoad(t, s)
			input := tt.CloneEnvelope(candidate)
			ctx := context.Background()
			expected := tt.Revision(0)
			switch mode {
			case "revision":
				input.Revision = 2
			case "corrupt":
				r := input.Receipts["init"]
				r.Fingerprint = "invalid"
				input.Receipts["init"] = r
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "history":
				if err := s.Commit(ctx, "board", 0, input); err != nil {
					t.Fatal(err)
				}
				before = serviceTestLoad(t, s)
				expected = 1
				input.Revision = 2
				r := input.Receipts["init"]
				r.Fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				input.Receipts["init"] = r
				next := r
				next.RequestID = "noop"
				next.CommittedRevision = 2
				next.Delta = tt.Delta{}
				input.Receipts["noop"] = next
			}
			if err := s.Commit(ctx, "board", expected, input); err == nil {
				t.Fatal("invalid commit succeeded")
			}
			if !reflect.DeepEqual(before, serviceTestLoad(t, s)) {
				t.Fatal("failed commit published partial state")
			}
		})
	}
	invalid := tt.CloneEnvelope(candidate)
	invalid.Snapshot.NextID = 1
	if _, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": invalid}); err == nil {
		t.Fatal("corrupt seed accepted")
	}
}

func TestMemoryMissingBoardsAreIndependent(t *testing.T) {
	s := serviceTestStore(t)
	empty := serviceTestLoad(t, s)
	if empty.Revision != 0 || empty.Snapshot.Initialized || empty.Snapshot.NextID != 1 || len(empty.Receipts) != 0 {
		t.Fatal("wrong absent board")
	}
	delete(empty.Snapshot.Nodes, tt.RootID)
	if len(serviceTestLoad(t, s).Snapshot.Nodes) != 1 {
		t.Fatal("missing snapshot shared")
	}
	candidate := serviceTestCandidate(t)
	if err := s.Commit(context.Background(), "board", 0, candidate); err != nil {
		t.Fatal(err)
	}
	other, err := s.Load(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if other.Revision != 0 {
		t.Fatal("board scope leaked")
	}
}
