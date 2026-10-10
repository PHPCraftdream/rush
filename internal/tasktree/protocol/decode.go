package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected field name")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return fields, nil
}

func readField(fields map[string]json.RawMessage, name string, dst any) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s must not be null", name)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func nonblank(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			continue
		}
		var value string
		if err := readField(fields, name, &value); err != nil {
			return err
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must be nonblank", name)
		}
	}
	return nil
}

func decodeDraft(raw json.RawMessage) (tasktree.Draft, error) {
	fields, err := object(raw)
	if err != nil {
		return tasktree.Draft{}, err
	}
	var draft tasktree.Draft
	if err := readField(fields, "kind", &draft.Kind); err != nil {
		return draft, err
	}
	allowed := map[string]bool{"kind": true, "title": true}
	switch draft.Kind {
	case tasktree.KindTask:
		allowed["active_form"] = true
	case tasktree.KindGroup:
		allowed["children"] = true
	default:
		return draft, fmt.Errorf("draft kind must be task or group")
	}
	for f := range fields {
		if !allowed[f] {
			return draft, fmt.Errorf("field %q is not allowed for %s draft", f, draft.Kind)
		}
	}
	if _, ok := fields["title"]; !ok {
		return draft, fmt.Errorf("draft title is required")
	}
	if err := nonblank(fields, "title"); err != nil {
		return draft, err
	}
	if err := readField(fields, "title", &draft.Title); err != nil {
		return draft, err
	}
	if err := readField(fields, "active_form", &draft.ActiveForm); err != nil {
		return draft, err
	}
	if _, ok := fields["children"]; ok {
		var children []json.RawMessage
		if err := readField(fields, "children", &children); err != nil {
			return draft, err
		}
		for i, child := range children {
			decoded, err := decodeDraft(child)
			if err != nil {
				return draft, fmt.Errorf("children[%d]: %w", i, err)
			}
			draft.Children = append(draft.Children, decoded)
		}
	}
	return draft, nil
}

func decode(raw json.RawMessage) (tasktree.Command, tasktree.Revision, error) {
	var command tasktree.Command
	var expected tasktree.Revision
	fields, err := object(raw)
	if err != nil {
		return command, expected, err
	}
	if err := readField(fields, "op", &command.Op); err != nil {
		return command, expected, err
	}
	spec, ok := findOperation(command.Op)
	if !ok {
		return command, expected, fmt.Errorf("unknown operation %q", command.Op)
	}
	allowed := spec.allowed()
	for f := range fields {
		if !allowed[f] {
			return command, expected, fmt.Errorf("field %q is not allowed for %s", f, command.Op)
		}
	}
	if command.Op != tasktree.OpView {
		if _, ok := fields["expected_revision"]; !ok {
			return command, expected, fmt.Errorf("explicit expected_revision is required, including zero")
		}
		if err := readField(fields, "expected_revision", &expected); err != nil {
			return command, expected, err
		}
	}
	for _, f := range spec.required {
		if _, ok := fields[f]; !ok {
			return command, expected, fmt.Errorf("%s is required", f)
		}
	}
	if err := nonblank(fields, "id", "text", "within_id", "parent_id", "before_id", "reason", "title"); err != nil {
		return command, expected, err
	}
	_, id := fields["id"]
	_, text := fields["text"]
	_, within := fields["within_id"]
	_, ids := fields["ids"]
	if id && text || within && !text {
		return command, expected, fmt.Errorf("use one id or exact text; within_id requires text")
	}
	if ids && (id || text || within) {
		return command, expected, fmt.Errorf("ids cannot be combined with a selector")
	}
	if spec.selector && !(ids && command.Op == tasktree.OpRemove) && !id && !text {
		return command, expected, fmt.Errorf("one id or exact text is required")
	}
	for _, field := range []struct {
		name string
		dst  any
	}{
		{"id", &command.Target.ID},
		{"text", &command.Target.Text},
		{"within_id", &command.Target.WithinID},
		{"parent_id", &command.ParentID},
		{"before_id", &command.BeforeID},
		{"reason", &command.Reason},
		{"title", &command.Title},
		{"active_form", &command.ActiveForm},
		{"ids", &command.RemoveIDs},
	} {
		if err := readField(fields, field.name, field.dst); err != nil {
			return command, expected, err
		}
	}
	if ids {
		if len(command.RemoveIDs) == 0 {
			return command, expected, fmt.Errorf("ids must be nonempty")
		}
		seen := make(map[tasktree.NodeID]bool)
		for _, id := range command.RemoveIDs {
			if strings.TrimSpace(string(id)) == "" || seen[id] {
				return command, expected, fmt.Errorf("ids must be nonblank and distinct")
			}
			seen[id] = true
		}
	}
	if command.Op == tasktree.OpEdit && command.Title == nil && command.ActiveForm == nil {
		return command, expected, fmt.Errorf("edit requires title and/or active_form")
	}
	if spec.drafts {
		_, list := fields["list"]
		_, items := fields["items"]
		if list == items {
			return command, expected, fmt.Errorf("supply exactly one nonempty list or items")
		}
		if list {
			var drafts []json.RawMessage
			if err := readField(fields, "list", &drafts); err != nil {
				return command, expected, err
			}
			for i, raw := range drafts {
				draft, err := decodeDraft(raw)
				if err != nil {
					return command, expected, fmt.Errorf("list[%d]: %w", i, err)
				}
				command.Drafts = append(command.Drafts, draft)
			}
		} else {
			var items []json.RawMessage
			if err := readField(fields, "items", &items); err != nil {
				return command, expected, err
			}
			for i, raw := range items {
				var title string
				if err := readField(map[string]json.RawMessage{"item": raw}, "item", &title); err != nil {
					return command, expected, fmt.Errorf("items[%d]: %w", i, err)
				}
				if strings.TrimSpace(title) == "" {
					return command, expected, fmt.Errorf("items[%d] must be nonblank", i)
				}
				command.Drafts = append(command.Drafts, tasktree.Draft{Kind: tasktree.KindTask, Title: title})
			}
		}
		if len(command.Drafts) == 0 {
			return command, expected, fmt.Errorf("list/items must be nonempty")
		}
	}
	return command, expected, nil
}
