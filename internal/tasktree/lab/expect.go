package lab

import (
	"fmt"
	"slices"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func compareValue[T comparable](name string, expected *T, actual T) error {
	if expected != nil && *expected != actual {
		return fmt.Errorf("%s: got %v, want %v", name, actual, *expected)
	}
	return nil
}

func compareExpectation(expected Expectation, result protocol.ToolResult, snapshot tasktree.Snapshot) error {
	if expected.IsError == nil {
		return fmt.Errorf("is_error is required")
	}
	if err := compareValue("is_error", expected.IsError, result.IsError); err != nil {
		return err
	}
	if expected.ProblemCode != "" {
		if result.Details.Problem == nil || result.Details.Problem.Code != expected.ProblemCode {
			return fmt.Errorf("problem_code: want %s, got %v", expected.ProblemCode, result.Details.Problem)
		}
	}
	if expected.ReceiptRevision != nil {
		if result.Details.Receipt == nil {
			return fmt.Errorf("receipt_revision: missing receipt")
		}
		if err := compareValue("receipt_revision", expected.ReceiptRevision, result.Details.Receipt.CommittedRevision); err != nil {
			return err
		}
	}
	checks := []error{
		compareValue("revision", expected.Revision, result.Details.Summary.Revision),
		compareValue("replayed", expected.Replayed, result.Details.Replayed),
		compareValue("active_id", expected.ActiveID, result.Details.Summary.ActiveID),
		compareValue("next_id", expected.NextID, result.Details.Summary.NextID),
		compareValue("progress", expected.Progress, result.Details.Summary.Progress),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	for _, expectedNode := range expected.Nodes {
		node, ok := snapshot.Nodes[expectedNode.ID]
		if !ok {
			return fmt.Errorf("node %s: missing live node", expectedNode.ID)
		}
		if err := compareNode(expectedNode, node); err != nil {
			return fmt.Errorf("node %s: %w", expectedNode.ID, err)
		}
	}
	for _, id := range expected.RemovedIDs {
		_, live := snapshot.Nodes[id]
		_, removed := snapshot.Tombstones[id]
		if live || !removed {
			return fmt.Errorf("removed_id %s: want tombstone without live node", id)
		}
	}
	for _, id := range expected.AbsentIDs {
		_, live := snapshot.Nodes[id]
		_, removed := snapshot.Tombstones[id]
		if live || removed {
			return fmt.Errorf("absent_id %s: exists as live node or tombstone", id)
		}
	}
	return nil
}

func compareNode(expected NodeExpectation, node tasktree.Node) error {
	checks := []error{
		compareValue("parent_id", expected.ParentID, node.ParentID),
		compareValue("kind", expected.Kind, node.Kind),
		compareValue("title", expected.Title, node.Title),
		compareValue("status", expected.Status, node.Status),
		compareValue("reason", expected.Reason, node.Reason),
		compareValue("active_form", expected.ActiveForm, node.ActiveForm),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	if expected.Children != nil && !slices.Equal(*expected.Children, node.Children) {
		return fmt.Errorf("children: got %v, want %v", node.Children, *expected.Children)
	}
	return nil
}
