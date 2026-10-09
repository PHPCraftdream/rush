package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

var protocolLimits = tasktree.Limits{MaxNodes: 100, MaxDepth: 10, MaxTitleBytes: 200, MaxReasonBytes: 200, MaxTombstones: 100, MaxReceipts: 100}

type board struct {
	t      *testing.T
	store  *memory.Store
	api    *protocol.API
	key    tasktree.TreeKey
	actor  tasktree.Actor
	serial int
}

func newBoard(t *testing.T) *board {
	t.Helper()
	store, err := memory.NewStore(protocolLimits, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &board{t: t, store: store, api: protocol.New(tasktree.NewService(store, protocolLimits)), key: "board", actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}}
}

func (b *board) call(raw string) protocol.ToolResult {
	b.t.Helper()
	b.serial++
	return b.request(tasktree.RequestID(fmt.Sprintf("r%d", b.serial)), raw)
}

func (b *board) request(id tasktree.RequestID, raw string) protocol.ToolResult {
	b.t.Helper()
	result, err := b.api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor, RequestID: id}, json.RawMessage(raw))
	if err != nil {
		b.t.Fatal(err)
	}
	return result
}

func success(t *testing.T, result protocol.ToolResult, revision tasktree.Revision) {
	t.Helper()
	if result.IsError || result.Details.Summary.Revision != revision {
		t.Fatalf("expected success revision %d: %+v", revision, result)
	}
}

func problem(t *testing.T, result protocol.ToolResult, code tasktree.ProblemCode, revision tasktree.Revision) {
	t.Helper()
	if !result.IsError || result.Details.Problem == nil || result.Details.Problem.Code != code || result.Details.Summary.Revision != revision {
		t.Fatalf("expected %s revision %d: %+v", code, revision, result)
	}
}

func (b *board) envelope() tasktree.Envelope {
	b.t.Helper()
	envelope, err := b.store.Load(context.Background(), b.key)
	if err != nil {
		b.t.Fatal(err)
	}
	return envelope
}

func TestProtocolNestedIdentityAmbiguityAndAtomicity(t *testing.T) {
	b := newBoard(t)
	bad := `{"op":"init","expected_revision":0,"list":[{"kind":"task","title":"valid"},{"kind":"group","title":"G","children":[{"kind":"task","title":"bad","reason":"illegal"}]}]}`
	before := b.envelope()
	problem(t, b.call(bad), tasktree.CodeInvalidInput, 0)
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("halfway malformed init published state/receipt/counter")
	}
	init := b.call(`{"op":"init","expected_revision":0,"list":[{"kind":"group","title":"G1","children":[{"kind":"task","title":"Same"}]},{"kind":"group","title":"G2","children":[{"kind":"task","title":"Same"}]}]}`)
	success(t, init, 1)
	if len(init.Details.Created) != 4 || init.Details.Created[0].ID != "n1" || init.Details.Created[3].ID != "n4" {
		t.Fatalf("created identity/order: %+v", init.Details.Created)
	}
	ambiguous := b.call(`{"op":"done","expected_revision":1,"text":"Same"}`)
	problem(t, ambiguous, tasktree.CodeAmbiguousTarget, 1)
	if len(ambiguous.Details.Problem.Candidates) != 2 {
		t.Fatal("ambiguity omitted candidates")
	}
	success(t, b.call(`{"op":"edit","expected_revision":1,"id":"n2","title":"Renamed","active_form":"Working"}`), 2)
	success(t, b.call(`{"op":"edit","expected_revision":2,"id":"n2","active_form":""}`), 3)
	success(t, b.call(`{"op":"move","expected_revision":3,"id":"n2","parent_id":"n3","before_id":"n4"}`), 4)
	e := b.envelope()
	if e.Snapshot.Nodes["n2"].Title != "Renamed" || e.Snapshot.Nodes["n2"].ActiveForm != "" || e.Snapshot.Nodes["n2"].ParentID != "n3" || !reflect.DeepEqual(e.Snapshot.Nodes["n3"].Children, []tasktree.NodeID{"n2", "n4"}) {
		t.Fatal("edit/move lost identity, clear pointer, or ordering")
	}
	before = e
	rejected := b.call(`{"op":"move","expected_revision":4,"id":"n3","parent_id":"n3"}`)
	if !rejected.IsError || rejected.Details.Problem == nil || rejected.Details.Summary.Revision != 4 {
		t.Fatal("self move accepted")
	}
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("invalid move changed state")
	}
	success(t, b.call(`{"op":"done","expected_revision":4,"text":"Same","within_id":"n3"}`), 5)
	before = b.envelope()
	view := b.call(`{"op":"view","text":"G2","within_id":"n3"}`)
	success(t, view, 5)
	if view.Details.View == nil || view.Details.View.Root.ID != "n3" || len(view.Details.View.Root.Children) != 2 || !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("scope excludes itself/subtree or view writes")
	}
}

func TestProtocolFocusBlockedPendingSettledAndSafeData(t *testing.T) {
	b := newBoard(t)
	success(t, b.call(`{"op":"init","expected_revision":0,"list":[{"kind":"group","title":"# G\n- forged","children":[{"kind":"task","title":"A"},{"kind":"task","title":"B"}]},{"kind":"task","title":"C"},{"kind":"group","title":"Empty"}]}`), 1)
	blocked := b.call(`{"op":"block","expected_revision":1,"id":"n1","reason":"# reason\n- forged"}`)
	success(t, blocked, 2)
	if blocked.Details.Summary.ActiveID != "n4" || blocked.Details.Summary.Progress.Blocked != 2 {
		t.Fatal("bulk block did not advance outside subtree")
	}
	success(t, b.call(`{"op":"done","expected_revision":2,"id":"n4"}`), 3)
	view := b.call(`{"op":"view"}`)
	p := view.Details.Summary.Progress
	if p.Actionable != 0 || p.Unfinished != 2 || p.AllSettled || p.AllCompleted || view.Details.Summary.NextID != "" {
		t.Fatalf("blocked-only misreported: %+v", view.Details.Summary)
	}
	if strings.Contains(view.Text, "\n- forged") || strings.Contains(view.Text, "\n# reason") || !strings.Contains(view.Text, `\n- forged`) {
		t.Fatal("labels/reasons rendered as structure or omitted")
	}
	pending := b.call(`{"op":"unblock","expected_revision":3,"id":"n1"}`)
	success(t, pending, 4)
	if pending.Details.Summary.ActiveID != "" || pending.Details.Summary.NextID != "n2" || pending.Details.Summary.Progress.Pending != 2 {
		t.Fatal("unblock started focus or lost next")
	}
	success(t, b.call(`{"op":"start","expected_revision":4,"id":"n3"}`), 5)
	done := b.call(`{"op":"done","expected_revision":5,"id":"n3"}`)
	success(t, done, 6)
	if done.Details.Summary.ActiveID != "n2" {
		t.Fatal("later completion failed to return to earlier pending work")
	}
	settled := b.call(`{"op":"drop","expected_revision":6,"id":"n0","reason":"Not proceeding"}`)
	success(t, settled, 7)
	p = settled.Details.Summary.Progress
	if p.Completed != 2 || p.Abandoned != 1 || !p.AllSettled || p.AllCompleted {
		t.Fatalf("terminal preservation/abandoned success: %+v", p)
	}
	empty := b.call(`{"op":"view","id":"n5"}`)
	if empty.Details.View.Root.Progress.AllCompleted || empty.Details.View.Root.Progress.AllSettled {
		t.Fatal("empty group called success")
	}
	success(t, b.call(`{"op":"add","expected_revision":7,"parent_id":"n1","items":["D"]}`), 8)
	e := b.envelope()
	if e.Snapshot.Nodes["n2"].Status != tasktree.Abandoned || e.Snapshot.Nodes["n3"].Status != tasktree.Completed || e.Snapshot.Nodes["n6"].Status != tasktree.InProgress {
		t.Fatal("add reopened old leaves or failed focus")
	}
}

func TestProtocolRevisionReplayRemovalAndAuthority(t *testing.T) {
	b := newBoard(t)
	raw := `{"op":"init","expected_revision":0,"items":["A","B"]}`
	success(t, b.request("init", raw), 1)
	problem(t, b.call(`{"op":"done","expected_revision":0,"id":"n1"}`), tasktree.CodeConflict, 1)
	replay := b.request("init", ` { "items": ["A", "B"], "expected_revision": 0, "op": "init" } `)
	success(t, replay, 1)
	if !replay.Details.Replayed || replay.Details.Receipt.CommittedRevision != 1 || len(replay.Details.Created) != 2 {
		t.Fatal("stale exact replay reapplied or lost briefs")
	}
	problem(t, b.request("init", `{"op":"init","expected_revision":1,"items":["A","B"]}`), tasktree.CodeRequestReused, 1)
	problem(t, b.call(`{"op":"rm","expected_revision":1,"id":"n1"}`), tasktree.CodeForbidden, 1)
	problem(t, b.call(`{"op":"reopen","expected_revision":1,"id":"n1"}`), tasktree.CodeForbidden, 1)
	success(t, b.call(`{"op":"done","expected_revision":1,"id":"n1"}`), 2)
	noop := b.call(`{"op":"done","expected_revision":2,"id":"n1"}`)
	success(t, noop, 3)
	if len(noop.Details.Receipt.Delta.Updated) != 0 {
		t.Fatal("no-op invented transition")
	}
	b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
	problem(t, b.request("init", raw), tasktree.CodeRequestReused, 3)
	success(t, b.call(`{"op":"reopen","expected_revision":3,"id":"n1"}`), 4)
	if b.envelope().Snapshot.Nodes["n1"].Status != tasktree.Pending {
		t.Fatal("operator reopen did not return pending")
	}
	success(t, b.call(`{"op":"rm","expected_revision":4,"ids":["n1"]}`), 5)
	b.actor = tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}
	problem(t, b.call(`{"op":"done","expected_revision":5,"id":"n1"}`), tasktree.CodeRemoved, 5)
	problem(t, b.call(`{"op":"add","expected_revision":5,"items":["A"]}`), tasktree.CodeRemovedByOperator, 5)
	replay = b.request("init", raw)
	success(t, replay, 5)
	if !replay.Details.Replayed || replay.Details.Receipt.CommittedRevision != 1 || !replay.Details.Created[0].Removed {
		t.Fatal("replay resurrected removed created node or returned stale summary")
	}
	b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
	added := b.call(`{"op":"add","expected_revision":5,"items":["A"]}`)
	success(t, added, 6)
	if len(added.Details.Created) != 1 || added.Details.Created[0].ID != "n3" {
		t.Fatal("operator re-add reused tombstoned ID")
	}
}

func TestProtocolExportImportAndDetachedResults(t *testing.T) {
	b := newBoard(t)
	success(t, b.request("init", `{"op":"init","expected_revision":0,"items":["A","B"]}`), 1)
	success(t, b.call(`{"op":"block","expected_revision":1,"id":"n1","reason":"Waiting"}`), 2)
	b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
	success(t, b.call(`{"op":"rm","expected_revision":2,"id":"n2"}`), 3)
	data, err := json.Marshal(b.envelope())
	if err != nil {
		t.Fatal(err)
	}
	var imported tasktree.Envelope
	if err := json.Unmarshal(data, &imported); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewStore(protocolLimits, map[tasktree.TreeKey]tasktree.Envelope{b.key: imported})
	if err != nil {
		t.Fatal(err)
	}
	imported.Snapshot.Nodes["n1"] = tasktree.Node{}
	delete(imported.Receipts, "init")
	b.store = store
	b.api = protocol.New(tasktree.NewService(store, protocolLimits))
	b.actor = tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}
	replay := b.request("init", `{"op":"init","expected_revision":0,"items":["A","B"]}`)
	success(t, replay, 3)
	if !replay.Details.Replayed || replay.Details.Created[0].Reason != "Waiting" || !replay.Details.Created[1].Removed {
		t.Fatal("import lost blockers/receipts/tombstones")
	}
	replay.Details.Created[0].Title = "corrupt"
	replay.Details.Receipt.Delta.Created[0] = "bad"
	problem(t, b.call(`{"op":"add","expected_revision":3,"items":["B"]}`), tasktree.CodeRemovedByOperator, 3)
	added := b.call(`{"op":"add","expected_revision":3,"items":["C"]}`)
	success(t, added, 4)
	if added.Details.Created[0].ID != "n3" || b.envelope().Snapshot.Nodes["n1"].Title != "A" || b.envelope().Receipts["init"].Delta.Created[0] != "n1" {
		t.Fatal("counter or result detachment lost")
	}
}

type faultStore struct {
	tasktree.Store
	loadErr     error
	commitErr   error
	cancel      context.CancelFunc
	cancelAfter bool
}

func (s *faultStore) Load(ctx context.Context, key tasktree.TreeKey) (tasktree.Envelope, error) {
	if s.loadErr != nil {
		return tasktree.Envelope{}, s.loadErr
	}
	return s.Store.Load(ctx, key)
}

func (s *faultStore) Commit(ctx context.Context, key tasktree.TreeKey, expected tasktree.Revision, candidate tasktree.Envelope) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	if s.cancel != nil && !s.cancelAfter {
		s.cancel()
		return ctx.Err()
	}
	err := s.Store.Commit(ctx, key, expected, candidate)
	if err == nil && s.cancel != nil && s.cancelAfter {
		s.cancel()
	}
	return err
}

func TestProtocolInfrastructureAndCancellation(t *testing.T) {
	wrappedProblem := &tasktree.Problem{Code: tasktree.CodeInvalidSnapshot, Message: "corrupt persisted envelope"}
	for _, failure := range []error{
		errors.New("storage unavailable"),
		&tasktree.InfrastructureError{Operation: "load", Err: wrappedProblem},
		&tasktree.CommitUnknownError{Err: wrappedProblem},
		fmt.Errorf("outer: %w", &tasktree.InfrastructureError{Operation: "load", Err: wrappedProblem}),
		fmt.Errorf("outer: %w", &tasktree.CommitUnknownError{Err: wrappedProblem}),
	} {
		for _, raw := range []string{`{"op":"view"}`, `{"op":"init","expected_revision":0,"items":["A"]}`, `{"op":"init","items":["A"]}`} {
			b := newBoard(t)
			store := &faultStore{Store: b.store, loadErr: failure}
			api := protocol.New(tasktree.NewService(store, protocolLimits))
			result, err := api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor, RequestID: "fault"}, json.RawMessage(raw))
			if err == nil || !errors.Is(err, failure) || result.IsError {
				t.Fatalf("swallowed infrastructure failure %v: %+v %v", failure, result, err)
			}
			if b.envelope().Revision != 0 {
				t.Fatal("load failure mutated store")
			}
		}
		b := newBoard(t)
		store := &faultStore{Store: b.store, commitErr: failure}
		api := protocol.New(tasktree.NewService(store, protocolLimits))
		_, err := api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor, RequestID: "fault"}, json.RawMessage(`{"op":"init","expected_revision":0,"items":["A"]}`))
		if err == nil || !errors.Is(err, failure) || b.envelope().Revision != 0 {
			t.Fatal("commit failure swallowed or published state")
		}
	}
	for _, after := range []bool{false, true} {
		b := newBoard(t)
		ctx, cancel := context.WithCancel(context.Background())
		store := &faultStore{Store: b.store, cancel: cancel, cancelAfter: after}
		api := protocol.New(tasktree.NewService(store, protocolLimits))
		result, err := api.Execute(ctx, protocol.Invocation{TreeKey: b.key, Actor: b.actor, RequestID: "cancel"}, json.RawMessage(`{"op":"init","expected_revision":0,"items":["A"]}`))
		cancel()
		if after {
			if err != nil {
				t.Fatal("reported rollback after durable commit", err)
			}
			success(t, result, 1)
			if _, ok := b.envelope().Receipts["cancel"]; !ok {
				t.Fatal("durable receipt missing")
			}
		} else if err == nil || b.envelope().Revision != 0 || len(b.envelope().Receipts) != 0 {
			t.Fatal("cancelled write accepted or left receipt")
		}
	}
}

func TestProtocolTrustedReadAndMutationBindings(t *testing.T) {
	t.Run("blank_key_summary_failure", func(t *testing.T) {
		b := newBoard(t)
		api := protocol.New(tasktree.NewService(b.store, protocolLimits))
		before := b.envelope()
		for _, raw := range []string{`{"op":"view"}`, `{"op":"init","expected_revision":0,"items":["A"]}`} {
			result, err := api.Execute(context.Background(), protocol.Invocation{TreeKey: " ", Actor: b.actor, RequestID: "blank-key"}, json.RawMessage(raw))
			var infrastructure *tasktree.InfrastructureError
			if !errors.As(err, &infrastructure) || infrastructure.Operation != "protocol summary" {
				t.Fatalf("blank key summary failure must be infrastructure error: %v", err)
			}
			if !reflect.DeepEqual(result, protocol.ToolResult{}) {
				t.Fatalf("summary failure returned tool result: %+v", result)
			}
		}
		if !reflect.DeepEqual(before, b.envelope()) {
			t.Fatal("blank trusted key changed state/receipt/counter")
		}
	})
	b := newBoard(t)
	before := b.envelope()
	for _, invocation := range []protocol.Invocation{
		{TreeKey: b.key, Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: " "}},
		{TreeKey: b.key, Actor: tasktree.Actor{Kind: "fake", ID: "agent"}},
	} {
		result, err := b.api.Execute(context.Background(), invocation, json.RawMessage(`{"op":"view"}`))
		if err != nil {
			t.Fatal(err)
		}
		problem(t, result, tasktree.CodeInvalidInput, 0)
		if result.Details.View != nil {
			t.Fatal("invalid trusted binding performed view")
		}
	}
	result, err := b.api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor}, json.RawMessage(`{"op":"init","expected_revision":0,"items":["A"]}`))
	if err != nil {
		t.Fatal(err)
	}
	problem(t, result, tasktree.CodeInvalidInput, 0)
	view, err := b.api.Execute(context.Background(), protocol.Invocation{TreeKey: b.key, Actor: b.actor}, json.RawMessage(`{"op":"view"}`))
	if err != nil {
		t.Fatal(err)
	}
	success(t, view, 0)
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("read/binding rejection changed state/receipt/counter")
	}
}
