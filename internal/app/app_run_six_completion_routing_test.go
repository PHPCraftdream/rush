package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pf2Event is the classification of a provider request by its LAST message.
// Kind is one of "title", "delegate", "child", "started", "completion" or
// "unknown"; callID is the async job id for started/completion events, index
// is the derived call-N / CHILD-WORK-N number, text is the last message's
// content, and detail describes unclassified requests.
type pf2Event struct {
	kind   string
	callID string
	index  int
	text   string
	detail string
}

// pf2Classify routes a provider request on its LAST message instead of the
// newest historical user message, so a request that already carries the next
// started tool result or completion notice is classified by the current
// event, not by the previous turn.
func pf2Classify(body []byte) pf2Event {
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return pf2Event{kind: "unknown", detail: "unparseable or empty provider request body"}
	}
	last := req.Messages[len(req.Messages)-1]
	text, _ := last.Content.(string)
	role := last.Role
	event := pf2Event{text: text}
	switch {
	case role == "user" && strings.Contains(text, "Generate a concise title"):
		// Title generation quotes the original prompt (agent_title.go), so
		// this must be classified before any prompt-text branch.
		event.kind = "title"
	case role == "tool" && batchStartedRootID(text) != "":
		// The started tool response is `batch root <uuid> started;
		// completions arrive as selected notices.` and pairs with its
		// tool-call id.
		event.kind = "started"
		event.callID = last.ToolCallID
		event.index = pf2CallIndex(last.ToolCallID)
	case role == "user" && rbCompletionCallID(text) != "":
		// Selected completions are model-visible batch completion texts
		// ("Async job <id> () finished.") with an EMPTY tool name; they
		// arrive as the request's LAST user message when the parent resumes.
		event.kind = "completion"
		event.callID = rbCompletionCallID(text)
		event.index = pf2CallIndex(rbCompletionCallID(text))
	case role == "user" && strings.TrimSpace(text) == "run six jobs":
		event.kind = "delegate"
	case role == "user" && pf2SystemPrompt(req) != "" &&
		strings.Contains(pf2SystemPrompt(req), rbChildSystemPromptMarker) &&
		pf2ChildWorkIndex(text) != 0:
		// The delegated task agent's system template uniquely says "Given
		// the user's prompt"; the child's prompt is exactly CHILD-WORK-N.
		event.kind = "child"
		event.index = pf2ChildWorkIndex(text)
	default:
		event.kind = "unknown"
		event.detail = fmt.Sprintf("role=%s %s", role, pf2Brief(text))
	}
	return event
}

// pf2SystemPrompt returns the content of the first system message of the
// request, or "" when there is none.
func pf2SystemPrompt(req wireRequest) string {
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			s, _ := msg.Content.(string)
			return s
		}
	}
	return ""
}

// pf2CallIndex extracts N from a "call-N" id and returns 0 on any failure.
func pf2CallIndex(callID string) int {
	rest, ok := strings.CutPrefix(callID, "call-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

// pf2ChildWorkIndex returns N for an exact "CHILD-WORK-N" prompt with N >= 1,
// and 0 for anything else.
func pf2ChildWorkIndex(text string) int {
	trimmed := strings.TrimSpace(text)
	rest, ok := strings.CutPrefix(trimmed, "CHILD-WORK-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// pf2Brief renders the first line of text, truncated to 120 characters with
// "..." appended when truncated.
func pf2Brief(text string) string {
	line := text
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	if len(line) > 120 {
		return line[:120] + "..."
	}
	return line
}
