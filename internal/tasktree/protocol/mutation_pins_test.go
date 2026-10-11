package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func TestMutationPinsSummaryHints(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress tasktree.Progress
		want     string
		absent   string
	}{
		{"blocked", tasktree.Progress{Total: 1, Blocked: 1, Unfinished: 1}, "Blocked work remains unfinished; no actionable task.", "All task leaves completed successfully"},
		{"abandoned", tasktree.Progress{Total: 1, Abandoned: 1, Settled: 1, AllSettled: true}, "Work settled with abandoned tasks, not all completed successfully.", "All task leaves completed successfully"},
		{"completed", tasktree.Progress{Total: 1, Completed: 1, Settled: 1, AllSettled: true, AllCompleted: true}, "All task leaves completed successfully.", "Work settled with abandoned tasks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := renderSummary(tasktree.Summary{Initialized: true, Progress: tc.progress})
			if !strings.Contains(text, tc.want) || strings.Contains(text, tc.absent) {
				t.Fatalf("wrong hint: %s", text)
			}
		})
	}
}

func TestMutationPinsDefinition(t *testing.T) {
	for _, actor := range []tasktree.ActorKind{tasktree.ActorAgent, tasktree.ActorOperator} {
		var schema struct {
			Branches []struct {
				Properties map[string]struct {
					Const tasktree.Operation `json:"const"`
				} `json:"properties"`
				Required []string `json:"required"`
				AllOf    []struct {
					OneOf []json.RawMessage `json:"oneOf"`
				} `json:"allOf"`
			} `json:"oneOf"`
		}
		if err := json.Unmarshal((&API{}).Definition(actor).InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		seen := map[tasktree.Operation]bool{}
		for _, branch := range schema.Branches {
			op := branch.Properties["op"].Const
			seen[op] = true
			has := func(field string) bool {
				for _, required := range branch.Required {
					if required == field {
						return true
					}
				}
				return false
			}
			if !has("op") || has("expected_revision") != (op != tasktree.OpView) {
				t.Fatalf("%s required=%v", op, branch.Required)
			}
			field := map[tasktree.Operation]string{tasktree.OpBlock: "reason", tasktree.OpDrop: "reason", tasktree.OpMove: "parent_id"}[op]
			if field != "" && !has(field) {
				t.Fatalf("%s missing required %s", op, field)
			}
			if op == tasktree.OpStart || op == tasktree.OpView {
				want := 2
				if op == tasktree.OpView {
					want = 3
				}
				if len(branch.AllOf) != 1 || len(branch.AllOf[0].OneOf) != want {
					t.Fatalf("%s selector alternatives=%v", op, branch.AllOf)
				}
			}
		}
		for _, op := range []tasktree.Operation{tasktree.OpRemove, tasktree.OpReopen} {
			if seen[op] != (actor == tasktree.ActorOperator) {
				t.Fatalf("actor=%s op=%s presence=%t", actor, op, seen[op])
			}
		}
	}
}
