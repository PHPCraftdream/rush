package tasktree_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
)

type serviceTestCancelLoadStore struct {
	tt.Store
	cancel context.CancelFunc
}

func (s *serviceTestCancelLoadStore) Load(ctx context.Context, key tt.TreeKey) (tt.Envelope, error) {
	e, err := s.Store.Load(ctx, key)
	s.cancel()
	return e, err
}
func TestServiceCancellationAfterLoadPreservesState(t *testing.T) {
	base := serviceTestStore(t)
	before := serviceTestLoad(t, base)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &serviceTestCancelLoadStore{Store: base, cancel: cancel}
	_, err := tt.NewService(store, serviceTestLimits()).Mutate(ctx, "board", serviceTestActor, "init", 0, serviceTestInit())
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
		t.Fatalf("cancellation changed stored state: %v", err)
	}
}

func TestServiceUnknownWithoutPublicationRetriesSameKey(t *testing.T) {
	base := serviceTestStore(t)
	before := serviceTestLoad(t, base)
	unknown := &tt.CommitUnknownError{Err: errors.New("connection lost before outcome observed")}
	fault := &serviceTestFaultStore{Store: base, failure: unknown}
	s := tt.NewService(fault, serviceTestLimits())
	_, err := s.Mutate(context.Background(), "board", serviceTestActor, "init", 0, serviceTestInit())
	var returned *tt.CommitUnknownError
	if !errors.As(err, &returned) || !errors.Is(err, unknown.Err) || !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
		t.Fatalf("unknown type lost or stored state changed: %v", err)
	}
	fault.failure = nil
	r := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	if r.Replayed || r.Summary.Revision != 1 || len(serviceTestLoad(t, base).Receipts) != 1 {
		t.Fatal("same-key reconciliation failed when original commit never published")
	}
}

func TestServiceReadsDoNotCommitOrNormalize(t *testing.T) {
	base := serviceTestStore(t)
	s := tt.NewService(base, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	serviceTestMutate(t, s, serviceTestActor, "block", 1, tt.Command{Op: tt.OpBlock, Target: tt.Selector{ID: tt.RootID}, Reason: "waiting"})
	serviceTestMutate(t, s, serviceTestActor, "unblock", 2, tt.Command{Op: tt.OpUnblock, Target: tt.Selector{ID: tt.RootID}})
	before := serviceTestLoad(t, base)
	for i := 0; i < 2; i++ {
		summary, err := s.Summary(context.Background(), "board")
		if err != nil {
			t.Fatal(err)
		}
		view, err := s.View(context.Background(), "board", tt.Selector{})
		if err != nil {
			t.Fatal(err)
		}
		if summary.Revision != 3 || view.Summary.Revision != 3 || summary.ActiveID != "" || summary.NextID != "n1" || view.Root.Children[0].Status != tt.Pending {
			t.Fatalf("read normalized pending focus: %+v %+v", summary, view)
		}
	}
	after := serviceTestLoad(t, base)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reads changed stored state")
	}
}
