package batchmeta

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// CompletionPresentation contains only the fields used to render a completion.
type CompletionPresentation struct {
	ToolCallID, ToolName, Content                      string
	IsError, TimedOut, Stopped, Cancelled, Interrupted bool
	TimeoutSeconds                                     int
}

// FormatAsyncCompletion makes a completed job visible to the model and UI.
// The TimedOut branch's wording matches
// docs/plans/2026-09-27-wake-tools-contract.md §5.1 verbatim.
func FormatAsyncCompletion(completion CompletionPresentation) string {
	content := tools.TruncateOutput(strings.TrimSpace(completion.Content))
	if content == "" {
		content = "(no output)"
	}
	if completion.TimedOut {
		return fmt.Sprintf("Async job %s (%s) timed out after %ds and was stopped. Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, completion.TimeoutSeconds, content)
	}
	if completion.Stopped {
		return fmt.Sprintf("Async job %s (%s) was stopped (batch_stop). Partial output before the stop:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Cancelled {
		return fmt.Sprintf("Async job %s (%s) was cancelled (session stopped). Partial output:\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	if completion.Interrupted {
		return fmt.Sprintf("Async job %s (%s) was interrupted: its host process ended before the job finished.\n\n%s",
			completion.ToolCallID, completion.ToolName, content)
	}
	status := "finished"
	if completion.IsError {
		status = "failed"
	}
	return fmt.Sprintf("Async job %s (%s) %s.\n\n%s",
		completion.ToolCallID, completion.ToolName, status, content)
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
	completion := CompletionPresentation{
		ToolCallID: row.ToolCallID,
		ToolName:   "",
		Content:    row.OutcomeSummary,
		IsError:    row.State == "failed" || row.OutcomeCode == "failed",
		TimedOut:   row.State == "timed_out",
		Stopped:    row.Cause == "stop" && !recovered,
		Cancelled:  row.Cause == "stop" && row.State == "cancelled",
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
