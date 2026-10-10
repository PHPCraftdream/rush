package tasktree_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func TestServiceActiveFormImportRejectsUnchanged(t *testing.T) {
	base := serviceTestStore(t)
	serviceTestMutate(t, tt.NewService(base, serviceTestLimits()), serviceTestActor, "init", 0, serviceTestInit())
	good := serviceTestLoad(t, base)
	for _, form := range []string{strings.Repeat("x", serviceTestLimits().MaxTitleBytes+1), strings.Repeat("x", 1<<20)} {
		bad := tt.CloneEnvelope(good)
		n := bad.Snapshot.Nodes["n1"]
		n.ActiveForm = form
		bad.Snapshot.Nodes["n1"] = n
		before := tt.CloneEnvelope(bad)
		serviceTestCode(t, tt.CheckEnvelope(bad, serviceTestLimits()), tt.CodeInvalidSnapshot)
		_, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": bad})
		serviceTestCode(t, err, tt.CodeInvalidSnapshot)
		corrupt := tt.NewService(&serviceTestCorruptStore{Store: base, envelope: bad}, serviceTestLimits())
		for _, call := range []func() error{
			func() error { _, err := corrupt.Summary(context.Background(), "board"); return err },
			func() error { _, err := corrupt.View(context.Background(), "board", tt.Selector{}); return err },
			func() error {
				_, err := corrupt.Mutate(context.Background(), "board", serviceTestActor, "new", 1, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
				return err
			},
		} {
			var infrastructure *tt.InfrastructureError
			if err := call(); !errors.As(err, &infrastructure) {
				t.Fatalf("invalid import not infrastructure: %v", err)
			}
		}
		if !reflect.DeepEqual(bad, before) || !reflect.DeepEqual(good, serviceTestLoad(t, base)) {
			t.Fatal("invalid import changed input or durable envelope")
		}
	}
}

func TestServiceOperatorReceiptReuseBeforeAuthorization(t *testing.T) {
	for _, op := range []tt.Operation{tt.OpRemove, tt.OpReopen} {
		t.Run(string(op), func(t *testing.T) {
			base := serviceTestStore(t)
			s := tt.NewService(base, serviceTestLimits())
			serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
			serviceTestMutate(t, s, serviceTestActor, "done", 1, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}})
			command := tt.Command{Op: op, Target: tt.Selector{ID: "n1"}}
			original := serviceTestMutate(t, s, serviceTestOperator, "operator", 2, command)
			before := serviceTestLoad(t, base)
			for _, actor := range []tt.Actor{serviceTestActor, {Kind: tt.ActorOperator, ID: "another/operator"}} {
				_, err := s.Mutate(context.Background(), "board", actor, "operator", 2, command)
				serviceTestCode(t, err, tt.CodeRequestReused)
				if !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
					t.Fatal("changed actor reuse wrote state")
				}
			}
			_, err := s.Mutate(context.Background(), "board", serviceTestActor, "fresh", 2, command)
			serviceTestCode(t, err, tt.CodeForbidden)
			if !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
				t.Fatal("unauthorized fresh request wrote state")
			}
			replay := serviceTestMutate(t, s, serviceTestOperator, "operator", 2, command)
			if !replay.Replayed || !reflect.DeepEqual(replay.Receipt, original.Receipt) || !reflect.DeepEqual(before, serviceTestLoad(t, base)) {
				t.Fatal("operator replay changed receipt or state")
			}
		})
	}
}

func TestServiceImportedBaselinesAndGappedHistory(t *testing.T) {
	base := serviceTestStore(t)
	serviceTestMutate(t, tt.NewService(base, serviceTestLimits()), serviceTestActor, "init", 0, serviceTestInit())
	baseline := serviceTestLoad(t, base)
	baseline.Revision = 40
	baseline.Receipts = nil
	seeded, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": baseline})
	if err != nil {
		t.Fatal(err)
	}
	s := tt.NewService(seeded, serviceTestLimits())
	command := tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n1"}}
	accepted := serviceTestMutate(t, s, serviceTestActor, "new", 40, command)
	replay := serviceTestMutate(t, s, serviceTestActor, "new", 40, command)
	if accepted.Summary.Revision != 41 || !replay.Replayed || !reflect.DeepEqual(accepted.Receipt, replay.Receipt) || len(serviceTestLoad(t, seeded).Receipts) != 1 {
		t.Fatal("imported baseline mutation/replay failed")
	}
	gapped := serviceTestLoad(t, seeded)
	gapped.Revision = 90
	r := gapped.Receipts["new"]
	r.CommittedRevision = 7
	gapped.Receipts["new"] = r
	old := r
	old.RequestID = "old"
	old.CommittedRevision = 2
	old.Delta = tt.Delta{}
	gapped.Receipts["old"] = old
	if err := tt.CheckEnvelope(gapped, serviceTestLimits()); err != nil {
		t.Fatal("gapped retained history rejected:", err)
	}
	resumed, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": gapped})
	if err != nil {
		t.Fatal(err)
	}
	serviceTestMutate(t, tt.NewService(resumed, serviceTestLimits()), serviceTestActor, "later", 90, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n2"}})
	after := serviceTestLoad(t, resumed)
	if after.Revision != 91 || len(after.Receipts) != 3 || !reflect.DeepEqual(after.Receipts["old"], old) || !reflect.DeepEqual(after.Receipts["new"], r) {
		t.Fatal("gapped history not conserved")
	}
}

func TestServiceGuardedUninitializedImport(t *testing.T) {
	baseline := tt.Envelope{Revision: 12, Snapshot: tt.EmptySnapshot()}
	baseline.Snapshot.TitleGuards = []tt.TitleGuard{{Kind: tt.KindTask, Title: "deleted legacy"}}
	seeded, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": baseline})
	if err != nil {
		t.Fatal(err)
	}
	s := tt.NewService(seeded, serviceTestLimits())
	r := serviceTestMutate(t, s, serviceTestActor, "init", 12, serviceTestInit())
	after := serviceTestLoad(t, seeded)
	if r.Summary.Revision != 13 || !after.Snapshot.Initialized || !reflect.DeepEqual(after.Snapshot.TitleGuards, baseline.Snapshot.TitleGuards) {
		t.Fatal("initialization lost legacy guards")
	}
	before := tt.CloneEnvelope(after)
	_, err = s.Mutate(context.Background(), "board", serviceTestActor, "guarded", 13, tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "deleted legacy"}}})
	serviceTestCode(t, err, tt.CodeRemovedByOperator)
	if !reflect.DeepEqual(before, serviceTestLoad(t, seeded)) {
		t.Fatal("guarded addition changed imported state")
	}
	baseline.Revision = 0
	if err := tt.CheckEnvelope(baseline, serviceTestLimits()); err == nil {
		t.Fatal("revision zero accepted guards")
	}
}

func TestServiceRemovalReceiptActorCorruption(t *testing.T) {
	base := serviceTestStore(t)
	s := tt.NewService(base, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	serviceTestMutate(t, s, serviceTestOperator, "rm", 1, tt.Command{Op: tt.OpRemove, Target: tt.Selector{ID: "n1"}})
	good := serviceTestLoad(t, base)
	for _, actor := range []tt.Actor{serviceTestActor, {Kind: tt.ActorOperator, ID: "other/operator"}} {
		bad := tt.CloneEnvelope(good)
		r := bad.Receipts["rm"]
		r.Actor = actor
		bad.Receipts["rm"] = r
		if err := tt.CheckEnvelope(bad, serviceTestLimits()); err == nil {
			t.Fatal("mismatched removal actor accepted")
		}
		if _, err := memory.NewStore(serviceTestLimits(), map[tt.TreeKey]tt.Envelope{"board": bad}); err == nil {
			t.Fatal("corrupt checkpoint seed accepted")
		}
		corrupt := tt.NewService(&serviceTestCorruptStore{Store: base, envelope: bad}, serviceTestLimits())
		for _, call := range []func() error{
			func() error { _, err := corrupt.Summary(context.Background(), "board"); return err },
			func() error { _, err := corrupt.View(context.Background(), "board", tt.Selector{}); return err },
			func() error {
				_, err := corrupt.Mutate(context.Background(), "board", serviceTestActor, "new", 2, tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n2"}})
				return err
			},
		} {
			var infrastructure *tt.InfrastructureError
			if err := call(); !errors.As(err, &infrastructure) {
				t.Fatalf("removal corruption not infrastructure: %v", err)
			}
		}
	}
}

func TestServiceRetainedHistoryOrderValidation(t *testing.T) {
	base := serviceTestStore(t)
	s := tt.NewService(base, serviceTestLimits())
	serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	serviceTestMutate(t, s, serviceTestOperator, "rm", 1, tt.Command{Op: tt.OpRemove, Target: tt.Selector{ID: "n1"}})
	good := serviceTestLoad(t, base)
	good.Revision = 80
	creation := good.Receipts["init"]
	creation.CommittedRevision = 10
	good.Receipts["init"] = creation
	removal := good.Receipts["rm"]
	removal.CommittedRevision = 30
	good.Receipts["rm"] = removal
	if err := tt.CheckEnvelope(good, serviceTestLimits()); err != nil {
		t.Fatal(err)
	}
	baseline := tt.CloneEnvelope(good)
	baseline.Receipts = nil
	if err := tt.CheckEnvelope(baseline, serviceTestLimits()); err != nil {
		t.Fatal("baseline tombstone requires fabricated history:", err)
	}
	for _, delta := range []tt.Delta{{Created: []tt.NodeID{"n2"}}, {Created: []tt.NodeID{"n1"}}, {Updated: []tt.NodeID{"n1"}}, {Removed: []tt.NodeID{"n1"}}} {
		bad := tt.CloneEnvelope(good)
		r := removal
		r.RequestID = "later"
		r.CommittedRevision = 60
		r.Delta = delta
		bad.Receipts["later"] = r
		if err := tt.CheckEnvelope(bad, serviceTestLimits()); err == nil {
			t.Fatalf("invalid retained ordering accepted: %+v", delta)
		}
	}
}
