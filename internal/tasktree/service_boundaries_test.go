package tasktree_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

func TestServiceActiveFormAtomicityAndEditPresence(t *testing.T) {
	limits := serviceTestLimits()
	limits.MaxTitleBytes = 8
	for _, form := range []string{"ééééx", strings.Repeat("x", 1<<20)} {
		t.Run("oversized", func(t *testing.T) {
			base := serviceTestStore(t)
			s := tt.NewService(base, limits)
			drafts := []tt.Draft{{Kind: tt.KindTask, Title: "guard"}, {Kind: tt.KindGroup, Title: "G", Children: []tt.Draft{{Kind: tt.KindTask, Title: "bad", ActiveForm: form}}}}
			reject := func(id tt.RequestID, command tt.Command, code tt.ProblemCode) {
				t.Helper()
				before := serviceTestLoad(t, base)
				reply, err := s.Mutate(context.Background(), "board", serviceTestOperator, id, before.Revision, command)
				serviceTestCode(t, err, code)
				if !reflect.DeepEqual(reply, tt.MutationReply{}) || !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
					t.Fatal("rejection changed snapshot, counter, guards, focus, revision or receipts")
				}
			}
			reject("init", tt.Command{Op: tt.OpInit, Drafts: drafts}, tt.CodeLimitExceeded)
			serviceTestMutate(t, s, serviceTestActor, "init", 0, tt.Command{Op: tt.OpInit, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "A"}, {Kind: tt.KindTask, Title: "guard"}}})
			serviceTestMutate(t, s, serviceTestOperator, "rm", 1, tt.Command{Op: tt.OpRemove, Target: tt.Selector{ID: "n2"}})
			reject("add", tt.Command{Op: tt.OpAdd, Drafts: drafts}, tt.CodeLimitExceeded)
			title := "guard"
			reject("edit", tt.Command{Op: tt.OpEdit, Target: tt.Selector{ID: "n1"}, Title: &title, ActiveForm: &form}, tt.CodeLimitExceeded)
			reject("empty", tt.Command{Op: tt.OpEdit, Target: tt.Selector{ID: "n1"}}, tt.CodeInvalidInput)
			_, err := s.Mutate(context.Background(), "board", serviceTestActor, "guarded", 2, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "guard"}}})
			serviceTestCode(t, err, tt.CodeRemovedByOperator)
		})
	}
	base := serviceTestStore(t)
	s := tt.NewService(base, limits)
	form, title := "éééé", "A"
	serviceTestMutate(t, s, serviceTestActor, "init", 0, tt.Command{Op: tt.OpInit, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: title, ActiveForm: form}}})
	command := tt.Command{Op: tt.OpEdit, Target: tt.Selector{ID: "n1"}, Title: &title, ActiveForm: &form}
	before := serviceTestLoad(t, base)
	noop := serviceTestMutate(t, s, serviceTestActor, "noop", 1, command)
	after := serviceTestLoad(t, base)
	if noop.Summary.Revision != 2 || !reflect.DeepEqual(noop.Receipt.Delta, tt.Delta{}) || !reflect.DeepEqual(before.Snapshot, after.Snapshot) || len(after.Receipts) != 2 {
		t.Fatal("explicit unchanged edit did not commit only a receipt/revision")
	}
	replay := serviceTestMutate(t, s, serviceTestActor, "noop", 1, command)
	if !replay.Replayed || !reflect.DeepEqual(replay.Receipt, noop.Receipt) || !reflect.DeepEqual(after, serviceTestLoad(t, base)) {
		t.Fatal("unchanged edit replay wrote state")
	}
	for i, value := range []string{" \t", ""} {
		command.ActiveForm = &value
		r := serviceTestMutate(t, s, serviceTestActor, tt.RequestID(value+"clear"), tt.Revision(2+i), command)
		stored := serviceTestLoad(t, base)
		if stored.Snapshot.Nodes["n1"].ActiveForm != value || r.Summary.ActiveID != "n1" || !reflect.DeepEqual(r.Receipt.Delta, tt.Delta{Updated: []tt.NodeID{"n1"}}) {
			t.Fatal("blank/empty edit did not preserve text or clear form")
		}
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
