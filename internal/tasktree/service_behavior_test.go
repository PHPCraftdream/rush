package tasktree_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func serviceTestLimits() tt.Limits {
	return tt.Limits{MaxNodes: 50, MaxDepth: 10, MaxTitleBytes: 100, MaxReasonBytes: 100, MaxTombstones: 50, MaxReceipts: 100}
}

var serviceTestActor = tt.Actor{Kind: tt.ActorAgent, ID: "agent/session"}
var serviceTestOperator = tt.Actor{Kind: tt.ActorOperator, ID: "operator/session"}

func serviceTestInit() tt.Command {
	return tt.Command{Op: tt.OpInit, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "A"}, {Kind: tt.KindTask, Title: "B"}}}
}
func serviceTestStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.NewStore(serviceTestLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func serviceTestMutate(t *testing.T, s *tt.Service, actor tt.Actor, id tt.RequestID, rev tt.Revision, cmd tt.Command) tt.MutationReply {
	t.Helper()
	r, err := s.Mutate(context.Background(), "board", actor, id, rev, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func serviceTestCode(t *testing.T, err error, code tt.ProblemCode) {
	t.Helper()
	var p *tt.Problem
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func serviceTestLoad(t *testing.T, s tt.Store) tt.Envelope {
	t.Helper()
	e, err := s.Load(context.Background(), "board")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestServiceReplayIdentityNoopAndCurrentBriefs(t *testing.T) {
	store := serviceTestStore(t)
	s := tt.NewService(store, serviceTestLimits())
	first := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	if first.Summary.Revision != 1 || !reflect.DeepEqual(first.Receipt.Delta.Created, []tt.NodeID{"n1", "n2"}) {
		t.Fatalf("bad init: %+v", first)
	}
	serviceTestMutate(t, s, serviceTestActor, "done", 1, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
	noop := serviceTestMutate(t, s, serviceTestActor, "noop", 2, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
	if noop.Summary.Revision != 3 || len(noop.Receipt.Delta.Updated) != 0 {
		t.Fatalf("no-op not durable: %+v", noop)
	}
	serviceTestMutate(t, s, serviceTestOperator, "remove", 3, tt.Command{Op: tt.OpRemove, Target: tt.Selector{ID: "n1"}})
	replay := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	if !replay.Replayed || replay.Receipt.CommittedRevision != 1 || replay.Summary.Revision != 4 || len(replay.Created) != 2 || !replay.Created[0].Removed {
		t.Fatalf("bad current replay: %+v", replay)
	}
	for _, change := range []struct {
		actor    tt.Actor
		expected tt.Revision
		command  tt.Command
	}{
		{tt.Actor{Kind: tt.ActorAgent, ID: "other"}, 0, serviceTestInit()},
		{serviceTestOperator, 0, serviceTestInit()},
		{serviceTestActor, 1, serviceTestInit()},
		{serviceTestActor, 0, tt.Command{Op: tt.OpInit, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "different"}}}},
	} {
		_, err := s.Mutate(context.Background(), "board", change.actor, "init", change.expected, change.command)
		serviceTestCode(t, err, tt.CodeRequestReused)
	}
	if e := serviceTestLoad(t, store); e.Revision != 4 || len(e.Receipts) != 4 {
		t.Fatalf("replay wrote: %+v", e)
	}
	first.Receipt.Delta.Created[0] = "nzzz"
	replay.Created[1].Title = "corrupt"
	if err := tt.CheckEnvelope(serviceTestLoad(t, store), serviceTestLimits()); err != nil {
		t.Fatal(err)
	}
}

type serviceTestBarrierStore struct {
	tt.Store
	arrived chan struct{}
	release chan struct{}
}

func (s *serviceTestBarrierStore) Load(ctx context.Context, key tt.TreeKey) (tt.Envelope, error) {
	e, err := s.Store.Load(ctx, key)
	if err == nil && e.Revision == 1 {
		s.arrived <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return tt.Envelope{}, ctx.Err()
		}
	}
	return e, err
}
func TestServiceConcurrentCASAndSameInvocation(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "distinct", true: "same"}[same], func(t *testing.T) {
			base := serviceTestStore(t)
			serviceTestMutate(t, tt.NewService(base, serviceTestLimits()), serviceTestActor, "init", 0, serviceTestInit())
			barrier := &serviceTestBarrierStore{Store: base, arrived: make(chan struct{}, 2), release: make(chan struct{})}
			s := tt.NewService(barrier, serviceTestLimits())
			type result struct {
				reply tt.MutationReply
				err   error
			}
			results := make(chan result, 2)
			for i := 0; i < 2; i++ {
				id := tt.RequestID("left")
				if i == 1 && !same {
					id = "right"
				}
				go func(id tt.RequestID) {
					r, err := s.Mutate(context.Background(), "board", serviceTestActor, id, 1, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: string(id)}}})
					results <- result{r, err}
				}(id)
			}
			<-barrier.arrived
			<-barrier.arrived
			close(barrier.release)
			a, b := <-results, <-results
			if same {
				if a.err != nil || b.err != nil || a.reply.Replayed == b.reply.Replayed {
					t.Fatalf("same invocation not reconciled: %+v %+v", a, b)
				}
			} else {
				if a.err == nil && b.err == nil || a.err != nil && b.err != nil {
					t.Fatalf("want one winner: %+v %+v", a, b)
				}
				if a.err != nil {
					serviceTestCode(t, a.err, tt.CodeConflict)
				} else {
					serviceTestCode(t, b.err, tt.CodeConflict)
				}
			}
			e := serviceTestLoad(t, base)
			if e.Revision != 2 || len(e.Receipts) != 2 || len(e.Snapshot.Nodes) != 4 {
				t.Fatalf("lost/duplicate write: %+v", e)
			}
			serviceTestMutate(t, tt.NewService(base, serviceTestLimits()), serviceTestActor, "later", 2, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "later"}}})
			if len(serviceTestLoad(t, base).Snapshot.Nodes) != 5 {
				t.Fatal("accepted earlier update disappeared")
			}
		})
	}
}

type serviceTestFaultStore struct {
	tt.Store
	before  func()
	after   func()
	failure error
	unknown bool
}

func (s *serviceTestFaultStore) Commit(ctx context.Context, key tt.TreeKey, rev tt.Revision, candidate tt.Envelope) error {
	if s.before != nil {
		s.before()
	}
	if s.failure != nil && !s.unknown {
		return s.failure
	}
	if err := s.Store.Commit(ctx, key, rev, candidate); err != nil {
		return err
	}
	if s.after != nil {
		s.after()
	}
	if s.unknown {
		return &tt.CommitUnknownError{Err: s.failure}
	}
	return nil
}
func TestServiceCommitFailuresAndCancellation(t *testing.T) {
	for _, mode := range []string{"failure", "before", "after", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			base := serviceTestStore(t)
			before := serviceTestLoad(t, base)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("storage offline")
			fault := &serviceTestFaultStore{Store: base}
			switch mode {
			case "failure":
				fault.failure = sentinel
			case "before":
				fault.before = cancel
			case "after":
				fault.after = cancel
			case "unknown":
				fault.unknown = true
				fault.failure = sentinel
			}
			s := tt.NewService(fault, serviceTestLimits())
			r, err := s.Mutate(ctx, "board", serviceTestActor, "init", 0, serviceTestInit())
			e := serviceTestLoad(t, base)
			switch mode {
			case "failure":
				var infrastructure *tt.InfrastructureError
				if !errors.As(err, &infrastructure) || !errors.Is(err, sentinel) || !reflect.DeepEqual(before, e) {
					t.Fatalf("failed commit not infrastructure or published: %v %+v", err, e)
				}
			case "before":
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(before, e) {
					t.Fatalf("canceled commit published: %v %+v", err, e)
				}
			case "after":
				if err != nil || r.Summary.Revision != 1 || e.Revision != 1 {
					t.Fatalf("invented rollback: %v %+v", err, r)
				}
			case "unknown":
				var unknown *tt.CommitUnknownError
				if !errors.As(err, &unknown) || !errors.Is(err, sentinel) || e.Revision != 1 {
					t.Fatalf("unknown lost: %v %+v", err, e)
				}
				replay := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
				if !replay.Replayed || !reflect.DeepEqual(replay.Receipt, e.Receipts["init"]) || !reflect.DeepEqual(e, serviceTestLoad(t, base)) {
					t.Fatal("unknown retry changed durable state")
				}
			}
		})
	}
	base := serviceTestStore(t)
	before := serviceTestLoad(t, base)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tt.NewService(base, serviceTestLimits()).Mutate(ctx, "board", serviceTestActor, "init", 0, serviceTestInit())
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
		t.Fatal("pre-cancel not respected")
	}
}

func TestServiceOperatorProtectionAndRestart(t *testing.T) {
	store := serviceTestStore(t)
	s := tt.NewService(store, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	serviceTestMutate(t, s, serviceTestActor, "block", 1, tt.Command{Op: tt.OpBlock, Target: tt.Selector{ID: "n1"}, Reason: "waiting"})
	serviceTestMutate(t, s, serviceTestOperator, "rm", 2, tt.Command{Op: tt.OpRemove, Target: tt.Selector{ID: "n2"}})
	e := serviceTestLoad(t, store)
	seeded, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": e})
	if err != nil {
		t.Fatal(err)
	}
	e.Snapshot.Nodes["n1"] = tt.Node{}
	e.Receipts["init"] = tt.Receipt{}
	s = tt.NewService(seeded, serviceTestLimits())
	replay := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	if !replay.Replayed || !replay.Created[1].Removed || replay.Created[0].Reason != "waiting" {
		t.Fatalf("restart lost history: %+v", replay)
	}
	_, err = s.Mutate(context.Background(), "board", serviceTestActor, "stale", 3, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n2"}})
	serviceTestCode(t, err, tt.CodeRemoved)
	_, err = s.Mutate(context.Background(), "board", serviceTestActor, "resurrect", 3, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "B"}}})
	serviceTestCode(t, err, tt.CodeRemovedByOperator)
	added := serviceTestMutate(t, s, serviceTestOperator, "readd", 3, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "B"}}})
	if !reflect.DeepEqual(added.Receipt.Delta.Created, []tt.NodeID{"n3"}) {
		t.Fatal("removed ID reused")
	}
	serviceTestMutate(t, s, serviceTestActor, "done", 4, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n3"}})
	_, err = s.Mutate(context.Background(), "board", serviceTestActor, "forbidden", 5, tt.Command{Op: tt.OpReopen, Target: tt.Selector{ID: "n3"}})
	serviceTestCode(t, err, tt.CodeForbidden)
	r := serviceTestMutate(t, s, serviceTestOperator, "reopen", 5, tt.Command{Op: tt.OpReopen, Target: tt.Selector{ID: "n3"}})
	if r.Summary.ActiveID != "" || r.Summary.NextID != "n3" {
		t.Fatalf("reopen selected focus: %+v", r)
	}
}

func TestServiceTrustedIdentityBeforeEffects(t *testing.T) {
	store := serviceTestStore(t)
	before := serviceTestLoad(t, store)
	s := tt.NewService(store, serviceTestLimits())
	for _, x := range []struct {
		key     tt.TreeKey
		actor   tt.Actor
		request tt.RequestID
	}{
		{"", serviceTestActor, "r"}, {"board", tt.Actor{Kind: tt.ActorAgent}, "r"}, {"board", tt.Actor{Kind: "forged", ID: "x"}, "r"}, {"board", serviceTestActor, " "},
	} {
		_, err := s.Mutate(context.Background(), x.key, x.actor, x.request, 0, serviceTestInit())
		serviceTestCode(t, err, tt.CodeInvalidInput)
		if !reflect.DeepEqual(before, serviceTestLoad(t, store)) {
			t.Fatal("invalid trusted context changed state")
		}
	}
	_, err := s.Summary(context.Background(), " ")
	serviceTestCode(t, err, tt.CodeInvalidInput)
	_, err = s.View(context.Background(), "", tt.Selector{})
	serviceTestCode(t, err, tt.CodeInvalidInput)
	if !reflect.DeepEqual(before, serviceTestLoad(t, store)) {
		t.Fatal("invalid read binding changed state")
	}
}
