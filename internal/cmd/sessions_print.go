package cmd

// Shared message-rendering helpers used by the sessions last / tail / show
// readers: message printing (text and NDJSON), finish-reason labelling,
// tool-call origin lookup, and duration formatting.

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/crush/internal/message"
)

// formatAgo returns a human-friendly "X ago" string for the given duration.
func formatAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		return fmt.Sprintf("%dd %dh ago", days, hours)
	}
}

// formatDurationShort returns a compact "Xm Ys" or "Xh Ym" string.
func formatDurationShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// toolCallOrigin captures the (name, raw JSON input) of an assistant
// ToolCall, indexed by its ToolCallID so that the subsequent
// ToolResult render can pull out the call's most useful argument
// (file_path, url, pattern, etc.) and show it next to the result.
// Populated by buildToolCallContext before a batch render.
type toolCallOrigin struct {
	name  string
	input string
}

// buildToolCallContext walks every message in a session and indexes every
// ToolCall by its ID. The map is then handed to printMessageWithTime so
// renderings of ToolResult parts can look up "what was the call about"
// and prefix the result preview with the argument (e.g. file_path for
// view, url for fetch). Walking is O(N+M) over messages and parts;
// caller pays this once per render batch.
func buildToolCallContext(msgs []message.Message) map[string]toolCallOrigin {
	out := make(map[string]toolCallOrigin, len(msgs))
	for _, m := range msgs {
		for _, part := range m.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok || tc.ID == "" {
				continue
			}
			out[tc.ID] = toolCallOrigin{name: tc.Name, input: tc.Input}
		}
	}
	return out
}

// lookupToolCallOrigin returns the (name, input) recorded for toolCallID,
// or ("", "") when the context is nil or the id is unknown. Safe to call
// with a nil map — callers that don't need origin enrichment (legacy
// paths) can pass nil and get the old behaviour from
// formatToolResultPreview.
func lookupToolCallOrigin(ctx map[string]toolCallOrigin, toolCallID string) (string, string) {
	if ctx == nil {
		return "", ""
	}
	o, ok := ctx[toolCallID]
	if !ok {
		return "", ""
	}
	return o.name, o.input
}

// printMessageWithTime prints a timestamp header followed by the message
// content. Only adds the header in text format when CreatedAt != 0.
// A blank line is printed between messages for readability. callCtx
// (optional, may be nil) maps ToolCallID to the originating ToolCall's
// name and JSON input — when present, ToolResult rendering uses it to
// show the call's most useful argument next to the result.
//
// followedByLater reports whether a newer message exists in the same
// session after this one. It only affects how a FinishReasonError row is
// labelled: a bare "(finished: error)" reads like the process died there,
// but if the session went on to produce more rows the error was transient
// and the turn was re-run (coordinator transient-retry, an orchestrator
// re-invocation, or a Phase-4 auto-resume) — see finishReasonLabel.
func printMessageWithTime(w io.Writer, msg message.Message, format string, now time.Time, callCtx map[string]toolCallOrigin, followedByLater bool) {
	if format == "text" && msg.CreatedAt != 0 {
		ts := time.Unix(msg.CreatedAt, 0)
		ago := now.Sub(ts)
		fmt.Fprintf(w, "[%s] (%s)\n", ts.Format("2006-01-02 15:04:05"), formatAgo(ago))
	}
	printMessage(w, msg, format, callCtx, followedByLater)
}

// finishReasonLabel renders the "(finished: …)" suffix for a message row.
// For a FinishReasonError that is NOT the session's final row, it appends a
// note that the turn was retried — the same underlying finish_reason="error"
// means "the process died here" only when nothing came after it. Without
// this, a transient, auto-retried failure is indistinguishable from a
// terminal one in `sessions last` / `sessions show`.
func finishReasonLabel(reason message.FinishReason, followedByLater bool) string {
	if reason == message.FinishReasonError && followedByLater {
		return string(reason) + " — retried, session continued"
	}
	return string(reason)
}

func printMessage(w io.Writer, msg message.Message, format string, callCtx map[string]toolCallOrigin, followedByLater bool) {
	if format == "ndjson" {
		type msgJSON struct {
			ID           string `json:"id"`
			Role         string `json:"role"`
			Preview      string `json:"preview"`
			FinishReason string `json:"finish_reason,omitempty"`
			// Retried is true for a finish_reason="error" row that was
			// followed by more messages in the session — i.e. the error was
			// transient and the turn was re-run, not a terminal death. Kept
			// as a separate boolean so consumers can still switch on the raw
			// finish_reason enum. Omitted (false) for every non-error row.
			Retried bool `json:"retried,omitempty"`
		}
		preview := ""
		for _, part := range msg.Parts {
			if tc, ok := part.(message.TextContent); ok {
				preview = truncate(tc.Text, 200)
				break
			}
		}
		finishReason := ""
		retried := false
		if f := msg.FinishPart(); f != nil {
			finishReason = string(f.Reason)
			retried = f.Reason == message.FinishReasonError && followedByLater
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(msgJSON{
			ID:           msg.ID,
			Role:         string(msg.Role),
			Preview:      preview,
			FinishReason: finishReason,
			Retried:      retried,
		})
	} else {
		// text format
		fmt.Fprintf(w, "[%s]\n", msg.Role)
		rendered := 0
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case message.TextContent:
				if p.Text == "" {
					continue
				}
				fmt.Fprintf(w, "%s\n", p.Text)
				rendered++
			case message.ReasoningContent:
				if p.Thinking == "" {
					continue
				}
				fmt.Fprintf(w, "[thinking] %s\n", truncatePreview(firstLine(p.Thinking), 200))
				rendered++
			case message.ToolCall:
				if preview := formatToolCallPreview(p.Name, p.Input); preview != "" {
					fmt.Fprintf(w, "[tool: %s] %s\n", p.Name, preview)
				} else {
					fmt.Fprintf(w, "[tool: %s]\n", p.Name)
				}
				rendered++
			case message.ToolResult:
				name := p.Name
				if name == "" {
					name = p.ToolCallID
				}
				originName, originInput := lookupToolCallOrigin(callCtx, p.ToolCallID)
				preview := formatToolResultPreview(p.Content, originName, originInput)
				prefix := "[tool-result: " + name + "]"
				if p.IsError {
					prefix += " ERROR"
				}
				if preview != "" {
					fmt.Fprintf(w, "%s %s\n", prefix, preview)
				} else {
					fmt.Fprintf(w, "%s\n", prefix)
				}
				rendered++
			}
		}
		if rendered == 0 {
			// No renderable parts yet — most often a streaming row that
			// hasn't flushed text, or an auto-checkpoint placeholder with
			// only a partial Finish. Saying so explicitly is friendlier
			// than leaving a bare role header.
			fmt.Fprintf(w, "(no content yet)\n")
		}
		if f := msg.FinishPart(); f != nil && f.Reason != "" {
			fmt.Fprintf(w, "(finished: %s)\n", finishReasonLabel(f.Reason, followedByLater))
		}
		fmt.Fprintf(w, "\n")
	}
}
