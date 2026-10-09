package tasktree_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
)

func TestServiceCommitUnknownWrappingConflictPreservesDurableReceipt(t *testing.T) {
	base := serviceTestStore(t)
	conflict := &tt.Problem{Code: tt.CodeConflict, Message: "uncertain transport outcome"}
	fault := &serviceTestFaultStore{Store: base, unknown: true, failure: conflict}
	s := tt.NewService(fault, serviceTestLimits())
	_, err := s.Mutate(context.Background(), "board", serviceTestActor, "init", 0, serviceTestInit())
	var unknown *tt.CommitUnknownError
	if !errors.As(err, &unknown) || !errors.Is(err, conflict) {
		t.Fatalf("nested conflict replaced unknown outcome: %v", err)
	}
	durable := serviceTestLoad(t, base)
	receipt, ok := durable.Receipts["init"]
	if !ok || durable.Revision != 1 || len(durable.Receipts) != 1 || receipt.CommittedRevision != 1 || receipt.Actor != serviceTestActor || !reflect.DeepEqual(receipt.Delta.Created, []tt.NodeID{"n1", "n2"}) {
		t.Fatalf("injected commit did not retain exact invocation: %+v", durable)
	}
	replay := serviceTestMutate(t, s, serviceTestActor, "init", 0, serviceTestInit())
	if !replay.Replayed || !reflect.DeepEqual(replay.Receipt, receipt) || replay.Summary.Revision != 1 {
		t.Fatalf("next invocation did not reconcile exact receipt: %+v", replay)
	}
	if !reflect.DeepEqual(durable, serviceTestLoad(t, base)) {
		t.Fatal("exact retry changed durable state")
	}
}

type serviceTestOrderedReuseStore struct {
	tt.Store
	winnerActor       tt.Actor
	winnerFingerprint string
	published         chan struct{}
}

func (s *serviceTestOrderedReuseStore) Commit(ctx context.Context, key tt.TreeKey, expected tt.Revision, candidate tt.Envelope) error {
	receipt := candidate.Receipts["shared"]
	winner := receipt.Actor == s.winnerActor && receipt.Fingerprint == s.winnerFingerprint
	if !winner {
		select {
		case <-s.published:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := s.Store.Commit(ctx, key, expected, candidate)
	if winner {
		close(s.published)
	}
	return err
}

func TestServiceConcurrentCASRequestReuse(t *testing.T) {
	for _, mode := range []string{"actor", "payload", "actor_and_payload"} {
		t.Run(mode, func(t *testing.T) {
			base := serviceTestStore(t)
			serviceTestMutate(t, tt.NewService(base, serviceTestLimits()), serviceTestActor, "init", 0, serviceTestInit())
			winnerCommand := tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "winner"}}}
			loserCommand := winnerCommand
			loserActor := serviceTestActor
			if mode != "payload" {
				loserActor = tt.Actor{Kind: tt.ActorAgent, ID: "different/session"}
			}
			if mode != "actor" {
				loserCommand = tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: "loser"}}}
			}
			reference := serviceTestStore(t)
			serviceTestMutate(t, tt.NewService(reference, serviceTestLimits()), serviceTestActor, "init", 0, serviceTestInit())
			winnerReceipt := serviceTestMutate(t, tt.NewService(reference, serviceTestLimits()), serviceTestActor, "shared", 1, winnerCommand).Receipt
			ordered := &serviceTestOrderedReuseStore{Store: base, winnerActor: serviceTestActor, winnerFingerprint: winnerReceipt.Fingerprint, published: make(chan struct{})}
			barrier := &serviceTestBarrierStore{Store: ordered, arrived: make(chan struct{}, 2), release: make(chan struct{})}
			s := tt.NewService(barrier, serviceTestLimits())
			type result struct {
				reply tt.MutationReply
				err   error
			}
			winnerResult := make(chan result, 1)
			loserResult := make(chan result, 1)
			go func() {
				r, err := s.Mutate(context.Background(), "board", serviceTestActor, "shared", 1, winnerCommand)
				winnerResult <- result{r, err}
			}()
			go func() {
				r, err := s.Mutate(context.Background(), "board", loserActor, "shared", 1, loserCommand)
				loserResult <- result{r, err}
			}()
			<-barrier.arrived
			<-barrier.arrived
			close(barrier.release)
			winner, loser := <-winnerResult, <-loserResult
			if winner.err != nil || winner.reply.Replayed || !reflect.DeepEqual(winner.reply.Receipt, winnerReceipt) {
				t.Fatalf("wrong winner: %+v", winner)
			}
			serviceTestCode(t, loser.err, tt.CodeRequestReused)
			if loser.reply.Replayed || loser.reply.Summary.Revision != 0 || len(loser.reply.Created) != 0 {
				t.Fatalf("loser exposed success: %+v", loser)
			}
			e := serviceTestLoad(t, base)
			if !reflect.DeepEqual(e, serviceTestLoad(t, reference)) {
				t.Fatal("reuse race differs from single accepted invocation")
			}
			if e.Revision != 2 || len(e.Receipts) != 2 || len(e.Snapshot.Nodes) != 4 || e.Snapshot.Nodes["n3"].Title != "winner" || !reflect.DeepEqual(e.Receipts["shared"], winnerReceipt) {
				t.Fatalf("reuse race corrupted publication: %+v", e)
			}
		})
	}
}
