package protocol

import (
	"encoding/json"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

type operationSpec struct {
	op               tasktree.Operation
	description      string
	fields           []string
	required         []string
	selector         bool
	optionalSelector bool
	drafts           bool
	operator         bool
}

var operations = []operationSpec{
	{op: tasktree.OpView, description: "Read a subtree without changing focus or revision; defaults to root.", fields: []string{"id", "text", "within_id"}, optionalSelector: true},
	{op: tasktree.OpInit, description: "Initialize once with exactly one nonempty list or items; selects earliest pending task.", fields: []string{"list", "items"}, drafts: true},
	{op: tasktree.OpAdd, description: "Append tasks/groups under parent_id (root by default); never replace or delete omitted tasks.", fields: []string{"list", "items", "parent_id"}, drafts: true},
	{op: tasktree.OpStart, description: "Start one pending task, switching the single focus.", selector: true},
	{op: tasktree.OpDone, description: "Complete one task; abandoned tasks cannot be completed.", selector: true},
	{op: tasktree.OpBlock, description: "Block open leaves in a task/group with a nonblank reason; preserve terminal leaves.", fields: []string{"reason"}, required: []string{"reason"}, selector: true},
	{op: tasktree.OpUnblock, description: "Return blocked leaves to pending without selecting focus.", selector: true},
	{op: tasktree.OpDrop, description: "Abandon open leaves with a nonblank reason, keeping them visible; abandoned is not success.", fields: []string{"reason"}, required: []string{"reason"}, selector: true},
	{op: tasktree.OpEdit, description: "Edit title and/or task active_form without changing identity or status; empty active_form clears it.", fields: []string{"title", "active_form"}, selector: true},
	{op: tasktree.OpMove, description: "Move one node to group parent_id, optionally before its direct child before_id; preserve identity/status.", fields: []string{"parent_id", "before_id"}, required: []string{"parent_id"}, selector: true},
	{op: tasktree.OpRemove, description: "Operator only: remove one selected subtree or nonempty ids atomically, preserving deletion guards.", fields: []string{"ids"}, selector: true, operator: true},
	{op: tasktree.OpReopen, description: "Operator only: return one terminal task to pending without selecting focus.", selector: true, operator: true},
}

func (s operationSpec) allowed() map[string]bool {
	out := map[string]bool{"op": true}
	if s.op != tasktree.OpView {
		out["expected_revision"] = true
	}
	for _, f := range s.fields {
		out[f] = true
	}
	if s.selector || s.optionalSelector {
		for _, f := range []string{"id", "text", "within_id"} {
			out[f] = true
		}
	}
	return out
}

func findOperation(op tasktree.Operation) (operationSpec, bool) {
	for _, s := range operations {
		if s.op == op {
			return s, true
		}
	}
	return operationSpec{}, false
}

func stringSchema(nonblank bool) map[string]any {
	m := map[string]any{"type": "string"}
	if nonblank {
		m["minLength"] = 1
		m["pattern"] = `\S`
	}
	return m
}

func exclusiveFields(a, b string) map[string]any {
	return map[string]any{"oneOf": []any{
		map[string]any{"required": []string{a}, "not": map[string]any{"required": []string{b}}},
		map[string]any{"required": []string{b}, "not": map[string]any{"required": []string{a}}},
	}}
}

func selectorSchema(optional bool) map[string]any {
	choices := []any{
		map[string]any{"required": []string{"id"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"text"}}, map[string]any{"required": []string{"within_id"}}}}},
		map[string]any{"required": []string{"text"}, "not": map[string]any{"required": []string{"id"}}},
	}
	if optional {
		choices = append(choices, map[string]any{"not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"id"}}, map[string]any{"required": []string{"text"}}, map[string]any{"required": []string{"within_id"}}}}})
	}
	return map[string]any{"oneOf": choices}
}

func (a *API) Definition(kind tasktree.ActorKind) Definition {
	var branches []any
	var description strings.Builder
	description.WriteString("Incremental tasks with stable IDs and exact text selectors (duplicates require ID or within_id). Actor, tree, and request ID are host-bound. View first; mutations require explicit expected_revision, including zero. After conflict, inspect current state and reconsider with a new host request ID; transport retries retain the same request and payload. Blocked work is unfinished, abandoned work is not success, empty groups are not completed work.\n")
	for _, s := range operations {
		if s.operator && kind != tasktree.ActorOperator {
			continue
		}
		props := map[string]any{}
		for field := range s.allowed() {
			switch field {
			case "op":
				props[field] = map[string]any{"const": s.op}
			case "expected_revision":
				props[field] = map[string]any{"type": "integer", "minimum": 0, "maximum": uint64(^uint64(0))}
			case "list":
				props[field] = map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"$ref": "#/$defs/draft"}}
			case "items":
				props[field] = map[string]any{"type": "array", "minItems": 1, "items": stringSchema(true)}
			case "ids":
				props[field] = map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": stringSchema(true)}
			default:
				props[field] = stringSchema(field != "active_form")
			}
		}
		required := append([]string{"op"}, s.required...)
		if s.op != tasktree.OpView {
			required = append(required, "expected_revision")
		}
		branch := map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false, "description": s.description}
		var constraints []any
		if s.drafts {
			constraints = append(constraints, exclusiveFields("list", "items"))
		}
		if s.op == tasktree.OpRemove {
			constraints = append(constraints, map[string]any{"oneOf": []any{
				map[string]any{"required": []string{"ids"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"id"}}, map[string]any{"required": []string{"text"}}, map[string]any{"required": []string{"within_id"}}}}},
				map[string]any{"allOf": []any{selectorSchema(false), map[string]any{"not": map[string]any{"required": []string{"ids"}}}}},
			}})
		} else if s.selector || s.optionalSelector {
			constraints = append(constraints, selectorSchema(s.optionalSelector))
		}
		if s.op == tasktree.OpEdit {
			constraints = append(constraints, map[string]any{"anyOf": []any{map[string]any{"required": []string{"title"}}, map[string]any{"required": []string{"active_form"}}}})
		}
		if len(constraints) > 0 {
			branch["allOf"] = constraints
		}
		branches = append(branches, branch)
		description.WriteString(string(s.op) + ": " + s.description + "\n")
	}
	draft := map[string]any{"oneOf": []any{
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "title"}, "properties": map[string]any{"kind": map[string]any{"const": "task"}, "title": stringSchema(true), "active_form": stringSchema(false)}},
		map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "title"}, "properties": map[string]any{"kind": map[string]any{"const": "group"}, "title": stringSchema(true), "children": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/draft"}}}},
	}}
	schema, _ := json.Marshal(map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "oneOf": branches, "$defs": map[string]any{"draft": draft}})
	return Definition{Name: "tasks", Description: description.String(), InputSchema: schema}
}
