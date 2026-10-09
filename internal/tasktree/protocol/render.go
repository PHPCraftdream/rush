package protocol

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
)

func safe(value string) string { return strconv.Quote(value) }

func pathText(path []string) string {
	parts := make([]string, len(path))
	for i, label := range path {
		parts[i] = safe(label)
	}
	return strings.Join(parts, " / ")
}

func renderSummary(s tasktree.Summary) string {
	p := s.Progress
	text := fmt.Sprintf("revision=%d total=%d pending=%d active=%d completed=%d blocked=%d abandoned=%d actionable=%d unfinished=%d settled=%d all_settled=%t all_completed=%t\n", s.Revision, p.Total, p.Pending, p.InProgress, p.Completed, p.Blocked, p.Abandoned, p.Actionable, p.Unfinished, p.Settled, p.AllSettled, p.AllCompleted)
	if s.ActiveID != "" {
		text += "focus=" + safe(string(s.ActiveID)) + " path=" + pathText(s.ActivePath) + "\n"
	}
	switch {
	case !s.Initialized:
		text += fmt.Sprintf("Next: init with nonempty list/items and expected_revision=%d.", s.Revision)
	case p.Total == 0:
		text += "No task leaves; empty groups are not completed work. Next: add tasks with the current expected_revision."
	case s.ActiveID != "":
		text += "Next: work on the focus, then done; or block/drop with a reason."
	case s.NextID != "":
		text += "No active task. Next: start id=" + safe(string(s.NextID)) + " path=" + pathText(s.NextPath) + " with the current expected_revision."
	case p.Blocked > 0:
		text += "Blocked work remains unfinished; no actionable task. Next: resolve a blocker, unblock its ID, then explicitly start."
	case p.AllCompleted:
		text += "All task leaves completed successfully. Next: view or add new work."
	case p.Abandoned > 0:
		text += "Work settled with abandoned tasks, not all completed successfully. Next: view reasons; operator may reopen or add new work."
	default:
		text += "Next: view the current board."
	}
	return text
}

func renderMutation(reply tasktree.MutationReply) string {
	var b strings.Builder
	fmt.Fprintf(&b, "receipt_revision=%d replayed=%t\n", reply.Receipt.CommittedRevision, reply.Replayed)
	b.WriteString(renderSummary(reply.Summary))
	for _, node := range reply.Created {
		fmt.Fprintf(&b, "\ncreated id=%s", safe(string(node.ID)))
		if node.Removed {
			b.WriteString(" removed=true")
			continue
		}
		fmt.Fprintf(&b, " parent=%s kind=%s title=%s", safe(string(node.ParentID)), safe(string(node.Kind)), safe(node.Title))
		if node.Status != "" {
			fmt.Fprintf(&b, " status=%s", safe(string(node.Status)))
		}
		if node.Reason != "" {
			fmt.Fprintf(&b, " reason=%s", safe(node.Reason))
		}
	}
	return b.String()
}

func renderView(view tasktree.View) string {
	var b strings.Builder
	b.WriteString(renderSummary(view.Summary))
	var visit func(tasktree.NodeView, int)
	visit = func(node tasktree.NodeView, depth int) {
		fmt.Fprintf(&b, "\n%sid=%s kind=%s title=%s", strings.Repeat("  ", depth), safe(string(node.ID)), safe(string(node.Kind)), safe(node.Title))
		if node.Kind == tasktree.KindTask {
			fmt.Fprintf(&b, " status=%s", safe(string(node.Status)))
			if node.ActiveForm != "" {
				fmt.Fprintf(&b, " active_form=%s", safe(node.ActiveForm))
			}
			if node.Reason != "" {
				fmt.Fprintf(&b, " reason=%s", safe(node.Reason))
			}
		} else {
			p := node.Progress
			fmt.Fprintf(&b, " leaves=%d completed=%d blocked=%d abandoned=%d all_completed=%t", p.Total, p.Completed, p.Blocked, p.Abandoned, p.AllCompleted)
		}
		for _, child := range node.Children {
			visit(child, depth+1)
		}
	}
	visit(view.Root, 0)
	return b.String()
}

func renderProblem(s tasktree.Summary, p *tasktree.Problem) string {
	action := "Correct the operation-specific fields and selectors; view current IDs and retry with explicit expected_revision and a new host request ID."
	switch p.Code {
	case tasktree.CodeConflict:
		action = "View current state, reconsider the command, and use its current expected_revision with a new host request ID; do not silently rebase."
	case tasktree.CodeAmbiguousTarget:
		action = "Choose a candidate id below, or exact text with within_id to disambiguate."
	case tasktree.CodeRemoved:
		action = "This ID was removed permanently. View live IDs; never reuse the removed ID."
	case tasktree.CodeRemovedByOperator:
		action = "Do not resurrect the operator-deleted label. Ask the operator for explicit re-add/rename or choose different authorized work."
	case tasktree.CodeInvalidTransition:
		action = "View task status; unblock blocked work before start; only the operator can reopen terminal work."
	case tasktree.CodeInvalidTargetKind:
		action = "View IDs and choose a task for leaf operations or a group for parent/scope operations."
	case tasktree.CodeLimitExceeded:
		action = "Reduce the requested addition/label/reason, or ask the host to archive and bind a new board; do not discard history."
	case tasktree.CodeUninitialized:
		action = fmt.Sprintf("Initialize once with nonempty list/items and expected_revision=%d.", s.Revision)
	case tasktree.CodeAlreadyInitialized:
		action = "View the board and use add/edit/move; init never replaces existing work."
	case tasktree.CodeRequestReused:
		action = "For transport replay retain the exact original actor/payload/revision; for new intent obtain a new host request ID."
	case tasktree.CodeForbidden:
		action = "Ask the operator for rm/reopen; agents may use drop with a reason instead of deleting."
	case tasktree.CodeNotFound:
		action = "View current live IDs and choose an existing exact target."
	}
	text := "error=" + safe(string(p.Code)) + " message=" + safe(p.Message) + "\n" + renderSummary(s) + "\nCorrection: " + action
	for _, candidate := range p.Candidates {
		text += "\ncandidate id=" + safe(string(candidate.ID)) + " path=" + pathText(candidate.Path) + " status=" + safe(string(candidate.Status))
	}
	return text
}
