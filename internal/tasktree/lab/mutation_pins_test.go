package lab

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func TestMutationCheckpointWriterCumulative(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w := boundedCheckpointWriter{writer: &out, remaining: 3}
	if n, err := w.Write([]byte("ab")); n != 2 || err != nil {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	if n, err := w.Write([]byte("cd")); n != 0 || err == nil || out.String() != "ab" {
		t.Fatalf("cumulative bound: n=%d err=%v output=%q", n, err, out.String())
	}
}

func TestMutationRetainedTextInclusive(t *testing.T) {
	t.Parallel()
	b := reportBudget{}
	if err := b.retain(protocol.ToolResult{Text: strings.Repeat("x", MaxRetainedTextBytes)}); err != nil {
		t.Fatalf("exact text bound: %v", err)
	}
	if err := b.retain(protocol.ToolResult{}); err != nil {
		t.Fatalf("empty result at capacity: %v", err)
	}
	if err := b.retain(protocol.ToolResult{Text: "x"}); err == nil {
		t.Fatal("accepted text overflow")
	}
}

func TestMutationPreflightCollectionCost(t *testing.T) {
	t.Parallel()
	b := preflightBudget{remaining: 10}
	b.collection(2, 5)
	if b.err != nil || b.remaining != 0 {
		t.Fatalf("collection cost: remaining=%d err=%v", b.remaining, b.err)
	}
	b.take(1)
	if b.err == nil {
		t.Fatal("accepted aggregate overflow")
	}
}

func TestMutationDuplicateLabels(t *testing.T) {
	t.Parallel()
	s := labTestScenario(labTestStep("same", "a", `{"op":"view"}`), labTestStep("same", "b", `{"op":"view"}`))
	if _, err := Run(context.Background(), s, nil); err == nil {
		t.Fatal("accepted duplicate step labels")
	}
}

func TestMutationModeEnvelopeUnchanged(t *testing.T) {
	t.Parallel()
	before := tasktree.Envelope{Revision: 1}
	after := tasktree.Envelope{Revision: 2}
	result := protocol.ToolResult{IsError: true}
	result.Details.Problem = &tasktree.Problem{Code: tasktree.CodeRequestReused}
	if err := compareMode(Step{Mode: "reuse"}, result, before, after); err == nil {
		t.Fatal("reuse accepted changed durable envelope")
	}
}

func TestMutationNewRequestNotReplayed(t *testing.T) {
	t.Parallel()
	result := protocol.ToolResult{}
	result.Details.Replayed = true
	if err := compareMode(Step{}, result, tasktree.Envelope{}, tasktree.Envelope{}); err == nil {
		t.Fatal("new request accepted replayed result")
	}
}

func TestMutationREPLLineBound(t *testing.T) {
	t.Parallel()
	s := labTestScenario()
	input := `{"request_id":"r","payload":{"op":"view"}}` + strings.Repeat(" ", MaxREPLLineBytes) + "\n"
	var out bytes.Buffer
	_, err := REPL(context.Background(), strings.NewReader(input), &out, REPLOptions{TreeKey: s.TreeKey, Limits: s.Limits, Actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "a"}})
	if err == nil || out.Len() != 0 {
		t.Fatalf("oversized transport line: err=%v output bytes=%d", err, out.Len())
	}
}

func TestMutationRemovedRequiresTombstone(t *testing.T) {
	t.Parallel()
	e := Expectation{IsError: labTestPtr(false), RemovedIDs: []tasktree.NodeID{"missing"}}
	if err := compareExpectation(e, protocol.ToolResult{}, tasktree.Snapshot{}); err == nil {
		t.Fatal("removed ID accepted without tombstone")
	}
}

func TestMutationPreflightInclusive(t *testing.T) {
	t.Parallel()
	b := preflightBudget{remaining: 3}
	b.take(3)
	if b.err != nil || b.remaining != 0 {
		t.Fatalf("exact preflight bound: remaining=%d err=%v", b.remaining, b.err)
	}
}
