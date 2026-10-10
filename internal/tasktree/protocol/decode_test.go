package protocol

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

func TestDecodeBoundary(t *testing.T) {
	invalid := []string{
		`{"op":"init","items":["A"]}`,
		`{"op":"init","expected_revision":0,"items":["A"],"list":[]}`,
		`{"op":"init","expected_revision":0,"items":[]}`,
		`{"op":"init","expected_revision":null,"items":["A"]}`,
		`{"op":"init","expected_revision":-1,"items":["A"]}`,
		`{"op":"init","expected_revision":0,"items":[null]}`,
		`{"op":"init","expected_revision":0,"items":["A"],"id":"n1"}`,
		`{"op":"init","expected_revision":0,"list":[{"kind":"task","title":"A"},{"kind":"group","title":"B","active_form":"bad"}]}`,
		`{"op":"init","expected_revision":0,"list":[{"kind":"task","title":"A","children":[]}]}`,
		`{"op":"init","expected_revision":0,"list":[{"kind":"group","title":"G","children":[{"kind":"task","title":"A","status":"completed"}]}]}`,
		`{"op":"view","expected_revision":0}`,
		`{"op":"view","id":"n1","text":"A"}`,
		`{"op":"view","within_id":"n0"}`,
		`{"op":"view","id":null}`,
		`{"op":"view","actor":"operator"}`,
		`{"op":"view","tree_key":"other"}`,
		`{"op":"view","request_id":"fake"}`,
		`{"op":"view"} {"op":"view"}`,
		`{"op":"view","op":"view"}`,
		`{"op":"done","expected_revision":1,"id":"n1","reason":"irrelevant"}`,
		`{"op":"done","expected_revision":1,"id":" "}`,
		`{"op":"block","expected_revision":1,"id":"n1","reason":" "}`,
		`{"op":"move","expected_revision":1,"id":"n1"}`,
		`{"op":"edit","expected_revision":1,"id":"n1"}`,
		`{"op":"edit","expected_revision":1,"id":"n1","active_form":null}`,
		`{"op":"rm","expected_revision":1,"ids":["n1"],"id":"n1"}`,
		`{"op":"rm","expected_revision":1,"ids":["n1","n1"]}`,
		`[]`, `null`,
	}
	limits := tasktree.Limits{MaxNodes: 100, MaxDepth: 10, MaxTitleBytes: 200, MaxReasonBytes: 200, MaxTombstones: 100, MaxReceipts: 100}
	store, err := memory.NewStore(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := New(tasktree.NewService(store, limits))
	ctx := context.Background()
	invocation := Invocation{TreeKey: "boundary", Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}, RequestID: "init"}
	initialized, err := api.Execute(ctx, invocation, json.RawMessage(`{"op":"init","expected_revision":0,"items":["A"]}`))
	if err != nil || initialized.IsError {
		t.Fatalf("init: %+v %v", initialized, err)
	}
	before, err := store.Load(ctx, invocation.TreeKey)
	if err != nil {
		t.Fatal(err)
	}
	invocation.RequestID = "invalid"
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := decode(json.RawMessage(raw)); err == nil {
				t.Fatal("accepted malformed/irrelevant payload")
			}
			result, err := api.Execute(ctx, invocation, json.RawMessage(raw))
			if err != nil || !result.IsError || result.Details.Problem == nil || result.Details.Problem.Code != tasktree.CodeInvalidInput || !reflect.DeepEqual(result.Details.Summary, initialized.Details.Summary) {
				t.Fatalf("invalid input/current summary: %+v %v", result, err)
			}
			after, err := store.Load(ctx, invocation.TreeKey)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejection changed envelope: %+v %v", after, err)
			}
		})
	}
	command, revision, err := decode(json.RawMessage(`{"op":"edit","expected_revision":0,"id":"n1","active_form":""}`))
	if err != nil || revision != 0 || command.Title != nil || command.ActiveForm == nil || *command.ActiveForm != "" {
		t.Fatalf("edit pointers lost: %+v %d %v", command, revision, err)
	}
	command, revision, err = decode(json.RawMessage(`{"op":"init","expected_revision":0,"list":[{"kind":"group","title":"G","children":[{"kind":"task","title":"A","active_form":"Doing A"}]}]}`))
	if err != nil || revision != 0 || len(command.Drafts) != 1 || len(command.Drafts[0].Children) != 1 || command.Drafts[0].Children[0].Kind != tasktree.KindTask {
		t.Fatalf("recursive draft rejected: %+v %v", command, err)
	}
}
