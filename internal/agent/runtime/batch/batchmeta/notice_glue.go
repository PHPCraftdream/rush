package batchmeta

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// FormatAsyncCompletion makes a completed job visible to the model and UI.
// The TimedOut branch's wording matches
// docs/plans/2026-09-27-wake-tools-contract.md §5.1 verbatim.
func FormatAsyncCompletion(completion batchdispatch.AsyncCompletion) string {
	content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
	if content == "" {
		content = "(no output)"
	}
	var formatted string
	if completion.TimedOut {
		formatted = fmt.Sprintf("Async job %s (%s) timed out after %ds and was stopped. Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, completion.TimeoutSeconds, content)
	} else if completion.Stopped {
		formatted = fmt.Sprintf("Async job %s (%s) was stopped (batch_stop). Partial output before the stop:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	} else if completion.Cancelled {
		formatted = fmt.Sprintf("Async job %s (%s) was cancelled (session stopped). Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	} else if completion.Interrupted {
		formatted = fmt.Sprintf("Async job %s (%s) was interrupted: its host process ended before the job finished.\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	} else {
		status := "finished"
		if completion.IsError {
			status = "failed"
		}
		formatted = fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
			completion.ToolCallID, completion.ToolName, status, content)
	}
	if completion.ChildSessionID != "" {
		formatted += fmt.Sprintf("\n\nchild_session_id=%s (resume with agent(resume_session_id=%s); inject_agent addresses a running child)", completion.ChildSessionID, completion.ChildSessionID)
	}
	return formatted
}

// BuildBatchNoticeMessageParams converts a pulled batch_nodes selected
// notice row into the history message's params, mirroring
// buildJobNoticeMessageParams. Batch notices carry NO tool name (batch
// leaves resolve by payload, not by tool identity) and no timeout seconds:
// the row's own notice_kind/state/cause and the root's outcome summary are
// the single source of truth.
func BuildBatchNoticeMessageParams(row sessionbatch.BatchNoticeRow) message.CreateMessageParams {
	// H4 W2: a recovered node (State interrupted / OutcomeCode host_lost)
	// is an interruption, not a finished job and not a stop: FormatAsync
	// Completion checks Stopped BEFORE Interrupted, so a Cause=="stop" node recovered to interrupted/host_lost must
	// not keep Stopped. Cancelled is kept as-is: State "interrupted" never
	// equals "cancelled", so a recovered node can never read as Cancelled.
	recovered := row.State == "interrupted" || row.OutcomeCode == "host_lost"
	completion := batchdispatch.AsyncCompletion{
		ToolCallID:     row.ToolCallID,
		ToolName:       "",
		ChildSessionID: row.ChildSessionID,
		Content:        row.OutcomeSummary,
		IsError:        row.State == "failed" || row.OutcomeCode == "failed",
		TimedOut:       row.State == "timed_out",
		Stopped:        row.Cause == "stop" && !recovered,
		Cancelled:      row.Cause == "stop" && row.State == "cancelled",
	}
	// The structured state and the retained summary are kept; no cause or
	// executor envelope is invented.
	if row.Cause == "host_closing" || recovered {
		completion.Interrupted = true
	}
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: FormatAsyncCompletion(completion)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		NoticeKind:          row.Kind,
		Origin:              origin,
	}
}
