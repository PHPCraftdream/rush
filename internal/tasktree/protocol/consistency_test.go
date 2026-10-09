package protocol_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

type barrierStore struct {
	tasktree.Store
	loaded   chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	arrivals int
}

func (s *barrierStore) Load(ctx context.Context, key tasktree.TreeKey) (tasktree.Envelope, error) {
	envelope, err := s.Store.Load(ctx, key)
	if err != nil {
		return envelope, err
	}
	s.mu.Lock()
	s.arrivals++
	wait := s.arrivals <= 2
	s.mu.Unlock()
	if wait {
		s.loaded <- struct{}{}
		<-s.release
	}
	return envelope, nil
}

func TestProtocolConcurrentWriters(t *testing.T) {
	b := newBoard(t)
	success(t, b.call(`{"op":"init","expected_revision":0,"items":["A"]}`), 1)
	store := &barrierStore{Store: b.store, loaded: make(chan struct{}, 2), release: make(chan struct{})}
	api := protocol.New(tasktree.NewService(store, protocolLimits))
	type outcome struct {
		result protocol.ToolResult
		err    error
	}
	results := make(chan outcome, 2)
	for _, label := range []string{"B", "C"} {
		go func(label string) {
			result, err := api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor, RequestID: tasktree.RequestID(label)}, json.RawMessage(fmt.Sprintf(`{"op":"add","expected_revision":1,"items":[%q]}`, label)))
			results <- outcome{result: result, err: err}
		}(label)
	}
	<-store.loaded
	<-store.loaded
	close(store.release)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		out := <-results
		if out.err != nil {
			t.Fatal(out.err)
		}
		if out.result.IsError {
			problem(t, out.result, tasktree.CodeConflict, 2)
			conflicts++
		} else {
			success(t, out.result, 2)
			wins++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	e := b.envelope()
	if e.Revision != 2 || len(e.Snapshot.Nodes) != 3 || len(e.Receipts) != 2 || e.Snapshot.Nodes["n1"].Title != "A" {
		t.Fatal("CAS lost accepted work or persisted rejected receipt")
	}
}

func TestProtocolLimitsAndBulkValidationAtomicity(t *testing.T) {
	limits := protocolLimits
	limits.MaxNodes = 3
	limits.MaxTitleBytes = 3
	store, err := memory.NewStore(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &board{t: t, store: store, api: protocol.New(tasktree.NewService(store, limits)), key: "limits", actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}}
	before := b.envelope()
	problem(t, b.call(`{"op":"init","expected_revision":0,"items":["A","long"]}`), tasktree.CodeLimitExceeded, 0)
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("late title-limit failure partially initialized")
	}
	success(t, b.call(`{"op":"init","expected_revision":0,"items":["ABC","B"]}`), 1)
	before = b.envelope()
	problem(t, b.call(`{"op":"add","expected_revision":1,"items":["C"]}`), tasktree.CodeLimitExceeded, 1)
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("node limit partially published")
	}
	b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
	problem(t, b.call(`{"op":"rm","expected_revision":1,"ids":["n1","n9"]}`), tasktree.CodeNotFound, 1)
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("multi-remove failed halfway after deleting first ID")
	}
}

type interleavedWriteStore struct {
	tasktree.Store
	initialized bool
}

func (s *interleavedWriteStore) Commit(ctx context.Context, key tasktree.TreeKey, expected tasktree.Revision, candidate tasktree.Envelope) error {
	if err := s.Store.Commit(ctx, key, expected, candidate); err != nil {
		return err
	}
	if !s.initialized && expected == 0 && candidate.Snapshot.Initialized {
		s.initialized = true
		_, err := tasktree.NewService(s.Store, protocolLimits).Mutate(ctx, key, tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}, "interleaved-remove", 1, tasktree.Command{Op: tasktree.OpRemove, Target: tasktree.Selector{ID: "n1"}})
		if err != nil {
			return err
		}
	}
	return nil
}

func TestProtocolMutationReplySurvivesInterleavedRemoval(t *testing.T) {
	b := newBoard(t)
	store := &interleavedWriteStore{Store: b.store}
	b.api = protocol.New(tasktree.NewService(store, protocolLimits))
	raw := `{"op":"init","expected_revision":0,"items":["A","B"]}`
	result := b.request("init", raw)
	success(t, result, 1)
	wantCreated := []tasktree.NodeBrief{
		{ID: "n1", ParentID: "n0", Kind: tasktree.KindTask, Title: "A", Status: tasktree.InProgress},
		{ID: "n2", ParentID: "n0", Kind: tasktree.KindTask, Title: "B", Status: tasktree.Pending},
	}
	wantProgress := tasktree.Progress{Total: 2, Pending: 1, InProgress: 1, Actionable: 2, Unfinished: 2}
	if result.Details.Receipt == nil || result.Details.Receipt.CommittedRevision != 1 || result.Details.Replayed || !reflect.DeepEqual(result.Details.Created, wantCreated) || result.Details.Summary.ActiveID != "n1" || result.Details.Summary.NextID != "n1" || result.Details.Summary.Progress != wantProgress {
		t.Fatalf("init reply mixed revisions: %+v", result.Details)
	}
	e := b.envelope()
	if e.Revision != 2 || len(e.Receipts) != 2 || e.Receipts["init"].CommittedRevision != 1 || e.Receipts["interleaved-remove"].CommittedRevision != 2 || len(e.Snapshot.Nodes) != 2 || e.Snapshot.Nodes["n2"].Status != tasktree.InProgress {
		t.Fatalf("interleaved removal was not persisted: %+v", e)
	}
	if _, live := e.Snapshot.Nodes["n1"]; live {
		t.Fatal("removed n1 still live")
	}
	if e.Snapshot.Tombstones["n1"].ID != "n1" {
		t.Fatal("removed n1 lacks tombstone")
	}
	if !reflect.DeepEqual(*result.Details.Receipt, e.Receipts["init"]) {
		t.Fatal("init returned a different receipt")
	}
	replay := b.request("init", raw)
	success(t, replay, 2)
	wantLive := tasktree.NodeBrief{ID: "n2", ParentID: "n0", Kind: tasktree.KindTask, Title: "B", Status: tasktree.InProgress}
	wantProgress = tasktree.Progress{Total: 1, InProgress: 1, Actionable: 1, Unfinished: 1}
	if !replay.Details.Replayed || replay.Details.Receipt == nil || !reflect.DeepEqual(*replay.Details.Receipt, e.Receipts["init"]) || len(replay.Details.Created) != 2 || replay.Details.Created[0].ID != "n1" || !replay.Details.Created[0].Removed || !reflect.DeepEqual(replay.Details.Created[1], wantLive) || replay.Details.Summary.ActiveID != "n2" || replay.Details.Summary.NextID != "n2" || replay.Details.Summary.Progress != wantProgress {
		t.Fatalf("old receipt replay did not project current removal: %+v", replay.Details)
	}
	if !reflect.DeepEqual(e, b.envelope()) {
		t.Fatal("replay changed committed board")
	}
}
