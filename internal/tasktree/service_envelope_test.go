package tasktree_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
)

func TestServiceEnvelopeReceiptValidation(t *testing.T) {
	store := serviceTestStore(t)
	s := tt.NewService(store, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	serviceTestMutate(t, s, serviceTestActor, "done", 1, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
	good := serviceTestLoad(t, store)
	cases := map[string]func(*tt.Envelope){
		"blank request": func(e *tt.Envelope) {
			r := e.Receipts["init"]
			delete(e.Receipts, "init")
			r.RequestID = " "
			e.Receipts[" "] = r
		},
		"key identity": func(e *tt.Envelope) { r := e.Receipts["init"]; r.RequestID = "another"; e.Receipts["init"] = r },
		"blank actor":  func(e *tt.Envelope) { r := e.Receipts["init"]; r.Actor.ID = " "; e.Receipts["init"] = r },
		"actor kind":   func(e *tt.Envelope) { r := e.Receipts["init"]; r.Actor.Kind = "forged"; e.Receipts["init"] = r },
		"uppercase fingerprint": func(e *tt.Envelope) {
			r := e.Receipts["init"]
			r.Fingerprint = strings.ToUpper(r.Fingerprint)
			e.Receipts["init"] = r
		},
		"short fingerprint": func(e *tt.Envelope) { r := e.Receipts["init"]; r.Fingerprint = "ab"; e.Receipts["init"] = r },
		"nonhex fingerprint": func(e *tt.Envelope) {
			r := e.Receipts["init"]
			r.Fingerprint = strings.Repeat("z", 64)
			e.Receipts["init"] = r
		},
		"zero receipt revision":   func(e *tt.Envelope) { r := e.Receipts["init"]; r.CommittedRevision = 0; e.Receipts["init"] = r },
		"future receipt revision": func(e *tt.Envelope) { r := e.Receipts["init"]; r.CommittedRevision = 3; e.Receipts["init"] = r },
		"duplicate revision":      func(e *tt.Envelope) { r := e.Receipts["init"]; r.CommittedRevision = 2; e.Receipts["init"] = r },
		"overlapping delta": func(e *tt.Envelope) {
			r := e.Receipts["init"]
			r.Delta.Updated = []tt.NodeID{"n1"}
			e.Receipts["init"] = r
		},
		"duplicate delta": func(e *tt.Envelope) {
			r := e.Receipts["init"]
			r.Delta.Created = []tt.NodeID{"n1", "n1"}
			e.Receipts["init"] = r
		},
		"unknown delta ID": func(e *tt.Envelope) {
			r := e.Receipts["done"]
			r.Delta.Updated = []tt.NodeID{"nzzz"}
			e.Receipts["done"] = r
		},
		"duplicate completed": func(e *tt.Envelope) {
			r := e.Receipts["done"]
			r.Delta.Completed = []tt.NodeID{"n1", "n1"}
			e.Receipts["done"] = r
		},
		"completed outside updated": func(e *tt.Envelope) {
			r := e.Receipts["done"]
			r.Delta.Completed = []tt.NodeID{"n0"}
			e.Receipts["done"] = r
		},
		"completed group": func(e *tt.Envelope) {
			r := e.Receipts["done"]
			r.Delta.Updated = []tt.NodeID{"n0"}
			r.Delta.Completed = []tt.NodeID{"n0"}
			e.Receipts["done"] = r
		},
		"removed live ID": func(e *tt.Envelope) {
			r := e.Receipts["done"]
			r.Delta = tt.Delta{Removed: []tt.NodeID{"n1"}}
			e.Receipts["done"] = r
		},
		"snapshot cycle": func(e *tt.Envelope) {
			n := e.Snapshot.Nodes["n0"]
			n.Children = append(n.Children, "n0")
			e.Snapshot.Nodes["n0"] = n
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			e := tt.CloneEnvelope(good)
			corrupt(&e)
			if err := tt.CheckEnvelope(e, serviceTestLimits()); err == nil {
				t.Fatal("corrupt envelope accepted")
			}
		})
	}
	limits := serviceTestLimits()
	limits.MaxReceipts = 1
	if err := tt.CheckEnvelope(good, limits); err == nil {
		t.Fatal("receipt cap ignored")
	}
	zero := tt.Envelope{Snapshot: tt.EmptySnapshot()}
	if err := tt.CheckEnvelope(zero, serviceTestLimits()); err != nil {
		t.Fatal(err)
	}
	zero.Revision = 1
	if err := tt.CheckEnvelope(zero, serviceTestLimits()); err != nil {
		t.Fatal("positive imported baseline rejected:", err)
	}
}

type serviceTestCorruptStore struct {
	tt.Store
	envelope tt.Envelope
	loadErr  error
}

func (s *serviceTestCorruptStore) Load(context.Context, tt.TreeKey) (tt.Envelope, error) {
	return tt.CloneEnvelope(s.envelope), s.loadErr
}

func TestServiceCorruptionIsInfrastructure(t *testing.T) {
	base := serviceTestStore(t)
	s := tt.NewService(base, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	bad := serviceTestLoad(t, base)
	r := bad.Receipts["init"]
	r.Fingerprint = "broken"
	bad.Receipts["init"] = r
	for _, loadErr := range []error{nil, &tt.Problem{Code: tt.CodeInvalidSnapshot, Message: "storage decode corruption"}} {
		s = tt.NewService(&serviceTestCorruptStore{Store: base, envelope: bad, loadErr: loadErr}, serviceTestLimits())
		for _, call := range []func() error{
			func() error { _, err := s.Summary(context.Background(), "board"); return err },
			func() error { _, err := s.View(context.Background(), "board", tt.Selector{}); return err },
			func() error {
				_, err := s.Mutate(context.Background(), "board", serviceTestActor, "init", 0, serviceTestInit())
				return err
			},
		} {
			var infrastructure *tt.InfrastructureError
			if err := call(); !errors.As(err, &infrastructure) {
				t.Fatalf("corruption not infrastructure: %v", err)
			}
		}
	}
}

func TestServiceTypedCanonicalFingerprintAndReceiptCap(t *testing.T) {
	store := serviceTestStore(t)
	limits := serviceTestLimits()
	limits.MaxReceipts = 1
	s := tt.NewService(store, limits)
	var a, b tt.Command
	if err := json.Unmarshal([]byte(`{"op":"init","drafts":[{"kind":"task","title":"A"}]}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{ "drafts": [{"title":"A", "children":[], "kind":"task"}], "op":"init" }`), &b); err != nil {
		t.Fatal(err)
	}
	serviceTestMutate(t, s, serviceTestActor, "init", 0, a)
	replayed := serviceTestMutate(t, s, serviceTestActor, "init", 0, b)
	if !replayed.Replayed {
		t.Fatal("equivalent decoded payload not replayed")
	}
	_, err := s.Mutate(context.Background(), "board", serviceTestActor, "new", 1, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
	serviceTestCode(t, err, tt.CodeLimitExceeded)
	if serviceTestLoad(t, store).Revision != 1 {
		t.Fatal("cap failure changed state")
	}
}

func TestServiceDetachedViewsAndClone(t *testing.T) {
	store := serviceTestStore(t)
	s := tt.NewService(store, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	good := serviceTestLoad(t, store)
	copy := tt.CloneEnvelope(good)
	n := copy.Snapshot.Nodes["n0"]
	n.Children[0] = "nzzz"
	r := copy.Receipts["init"]
	r.Delta.Created[0] = "nzzz"
	delete(copy.Snapshot.Nodes, "n1")
	delete(copy.Receipts, "init")
	if err := tt.CheckEnvelope(good, serviceTestLimits()); err != nil {
		t.Fatal("clone aliases source:", err)
	}
	view, err := s.View(context.Background(), "board", tt.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	view.Root.Children[0].Title = "corrupt"
	view.Summary.ActivePath[0] = "corrupt"
	summary, err := s.Summary(context.Background(), "board")
	if err != nil {
		t.Fatal(err)
	}
	summary.ActivePath[0] = "corrupt"
	fresh, err := s.View(context.Background(), "board", tt.Selector{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Root.Children[0].Title != "A" || fresh.Summary.ActivePath[0] == "corrupt" {
		t.Fatal("read escaped mutable state")
	}
}
