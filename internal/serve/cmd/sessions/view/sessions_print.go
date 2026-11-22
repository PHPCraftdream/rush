package sessview

// Shared message-rendering helpers used by the sessions last / tail / show
// readers: message printing (text and NDJSON), finish-reason labelling,
// tool-call origin lookup, and duration formatting.

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// ToolCallOrigin captures the (name, raw JSON input) of an assistant
// ToolCall, indexed by its ToolCallID so that the subsequent
// ToolResult render can pull out the call's most useful argument
// (file_path, url, pattern, etc.) and show it next to the result.
// Populated by BuildToolCallContext before a batch render.
type ToolCallOrigin struct {
	Name  string
	Input string
}

// BuildToolCallContext walks every message in a session and indexes every
// ToolCall by its ID. The map is then handed to PrintMessageWithTime so
// renderings of ToolResult parts can look up "what was the call about"
// and prefix the result preview with the argument (e.g. file_path for
// view, url for fetch). Walking is O(N+M) over messages and parts;
// caller pays this once per render batch.
func BuildToolCallContext(msgs []message.Message) map[string]ToolCallOrigin {
	out := make(map[string]ToolCallOrigin, len(msgs))
	for _, m := range msgs {
		for _, part := range m.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok || tc.ID == "" {
				continue
			}
			out[tc.ID] = ToolCallOrigin{Name: tc.Name, Input: tc.Input}
		}
	}
	return out
}

// lookupToolCallOrigin returns the (name, input) recorded for toolCallID,
// or ("", "") when the context is nil or the id is unknown. Safe to call
// with a nil map — callers that don't need origin enrichment (legacy
// paths) can pass nil and get the old behaviour from
// formatToolResultPreview.
func lookupToolCallOrigin(ctx map[string]ToolCallOrigin, toolCallID string) (string, string) {
	if ctx == nil {
		return "", ""
	}
	o, ok := ctx[toolCallID]
	if !ok {
		return "", ""
	}
	return o.Name, o.Input
}

// PrintMessageWithTime prints a timestamp header followed by the message
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
func PrintMessageWithTime(w io.Writer, msg message.Message, format string, now time.Time, callCtx map[string]ToolCallOrigin, followedByLater bool) {
	if format == "text" && msg.CreatedAt != 0 {
		ts := time.Unix(msg.CreatedAt, 0)
		ago := now.Sub(ts)
		fmt.Fprintf(w, "[%s] (%s)\n", ts.Format("2006-01-02 15:04:05"), sesskit.FormatAgo(ago))
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

func printMessage(w io.Writer, msg message.Message, format string, callCtx map[string]ToolCallOrigin, followedByLater bool) {
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
				preview = cliout.Truncate(tc.Text, 200)
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

// IndexByID returns the index of the message with the given id in
// messages, or -1 if id is empty or not found (including the case where a
// previously-printed message no longer appears in a fresh List() result —
// matching the old findByID+isAfter behavior of treating a missing anchor
// as "print everything").
func IndexByID(messages []message.Message, id string) int {
	if id == "" {
		return -1
	}
	for i := range messages {
		if messages[i].ID == id {
			return i
		}
	}
	return -1
}
