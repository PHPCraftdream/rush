package app

// The flag-ON batch fixtures' model-request router. Unlike the previous,
// consumption-by-key router it replaces, this router is STATELESS with
// respect to routing: it
// keys only on the LAST message of the request body, so a retried request
// (a transient 403 on the child's reaction turn, for example) routes to the
// SAME phase again — which is exactly what the retry contract requires.
// Cross-request state exists only for VALIDATION (a phase that must happen
// at most once is reported through seqErr, never silently deduped).
//
// Ground truth the routing relies on (all verified against production):
//   - completions are model-visible "Async job <id> () finished." /
//     " failed." / " was stopped (job_kill). ..." texts (FormatAsyncCompletion
//     in coordinator_background.go), and batch notices carry an EMPTY tool
//     name; cancelled state lives in the batch_nodes DB row, never the text;
//   - started is persisted twice: as the tool result AND as a user history
//     message with the same text (batchStartedNoticeText);
//   - the delegated task agent's system template (task.md.tpl) uniquely says
//     "Given the user's prompt"; neither coder.md.tpl nor the title prompt
//     does, so the system prompt identifies the ACTOR.

import (
	"encoding/json"
	"strings"
	"sync"
)

// closeOnce closes c exactly once through o, so handler branches, helper
// goroutines and t.Cleanup can all race to close the same channel safely.
func closeOnce(o *sync.Once, c chan struct{}) {
	o.Do(func() { close(c) })
}

// rbLastMessage returns the role, text and tool call id of the LAST message.
// ok is false when the body does not unmarshal or carries no messages.
func rbLastMessage(body []byte) (role, text, callID string, ok bool) {
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return "", "", "", false
	}
	m := req.Messages[len(req.Messages)-1]
	text, _ = m.Content.(string)
	return m.Role, text, m.ToolCallID, true
}

// rbChildSystemPromptMarker is a phrase of the task agent's system template
// (internal/agent/templates/task.md.tpl) that neither the coder template nor
// the title prompt contains, so it identifies the delegated child actor.
const rbChildSystemPromptMarker = "Given the user's prompt"

// rbRouter routes batch fixture requests statelessly on the LAST message and
// VALIDATES (never dedupes) the phase sequence per actor.
type rbRouter struct {
	mu sync.Mutex
	// seen counts routed phases per actor, key "actor|phase". Counts are
	// validation-only: routing never consults them.
	seen map[string]int
}

func newRBRouter() *rbRouter {
	return &rbRouter{seen: make(map[string]int)}
}

// route returns the phase route for the request and a non-empty seqErr when
// the request is out of sequence. On a validation failure the route string is
// still returned as computed, so a caller that ignores seqErr behaves
// statelessly. Completions are recorded (for the root:done check) but never
// deduped: a 403 retry resends the same body and routes to the same
// completion phase again — that is correct.
func (r *rbRouter) route(body []byte) (route string, seqErr string) {
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "root:delegate", "unparseable provider request body"
	}
	system := ""
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		s, _ := m.Content.(string)
		if strings.Contains(s, "short title") {
			return "title", ""
		}
		system = s
	}
	actor := "root"
	if strings.Contains(system, rbChildSystemPromptMarker) {
		actor = "child"
	}
	lastRole, text, callID, ok := rbLastMessage(body)
	if !ok {
		return "root:delegate", "provider request carries no messages"
	}
	switch lastRole {
	case "user":
		switch {
		case rbCompletionCallID(text) != "":
			id := rbCompletionCallID(text)
			switch {
			case actor == "child" && id == "call-bash":
				route = "child:final"
			case actor == "root" && id == "call-agent":
				route = "root:final"
			default:
				route = actor + ":final"
				seqErr = "unexpected completion actor=" + actor + " id=" + id
			}
		case strings.HasPrefix(text, "batch root ") &&
			strings.Contains(text, "started; completions arrive as selected notices."):
			// The started announcement is also a user history message; the
			// child/root just yield until the completion arrives.
			if actor == "child" {
				route = "child:wait"
			} else {
				route = "root:wait"
			}
		case actor == "child" && strings.Contains(text, "WORKER-MARKER"):
			route = "child:bash"
		case actor == "root":
			route = "root:delegate"
		default:
			seqErr = "unexpected user last message"
		}
	case "tool":
		switch {
		case callID == "call-stop":
			route = "child:stop"
		case callID == "call-bash" && batchStartedRootID(text) != "":
			route = "child:started"
		case callID == "call-agent" && batchStartedRootID(text) != "":
			route = "root:yield"
		default:
			seqErr = "unexpected tool last message call=" + callID
		}
	case "assistant":
		// The assistant answered, so the previous tool/user event can never
		// replay: route on the answer text.
		switch {
		case strings.Contains(text, "stop intent"):
			route = "child:stopAck"
		case strings.Contains(text, "child waiting for build") || strings.Contains(text, "child yielded"):
			route = "child:wait"
		case strings.Contains(text, "root-yield") || strings.Contains(text, "root yielded") ||
			strings.Contains(text, "waiting for child completion") || strings.Contains(text, "root waiting"):
			route = "root:wait"
		case strings.Contains(text, "root final answer"):
			route = "root:done"
		default:
			seqErr = "unexpected assistant last message"
		}
	default:
		seqErr = "unexpected last message role=" + lastRole
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	fail := func(msg string) {
		if seqErr == "" {
			seqErr = msg
		}
	}
	switch route {
	case "child:bash":
		if r.seen["child|bash"] > 0 {
			fail("child bash re-issued")
		}
		r.seen["child|bash"]++
	case "root:delegate":
		// root:delegate is valid only as the FIRST root event.
		for key := range r.seen {
			if strings.HasPrefix(key, "root|") {
				fail("root delegate re-issued")
				break
			}
		}
		r.seen["root|delegate"]++
	case "child:started":
		if r.seen["child|started"] > 0 {
			fail("child started re-issued")
		}
		r.seen["child|started"]++
	case "root:yield":
		if r.seen["root|yield"] > 0 {
			fail("root started re-issued")
		}
		r.seen["root|yield"]++
	case "root:final":
		r.seen["root|final"]++
	case "root:done":
		if r.seen["root|final"] == 0 {
			fail("root done before the root completion was routed")
		}
	}
	return route, seqErr
}

// rbCompletionCallID parses the real model-visible batch completion text
// (FormatAsyncCompletion) with the EMPTY tool name batch notices carry, and
// returns the batch root's tool call id. The cancelled state is established
// through the DB, not the text: Stopped is formatted before Cancelled, so a
// stopped root reads "was stopped (job_kill)".
func rbCompletionCallID(text string) string {
	rest, ok := strings.CutPrefix(text, "Async job ")
	if !ok {
		return ""
	}
	id, tail, ok := strings.Cut(rest, " () ")
	if !ok {
		return ""
	}
	if strings.HasPrefix(tail, "finished.") ||
		strings.HasPrefix(tail, "failed.") ||
		strings.HasPrefix(tail, "was stopped (job_kill).") {
		return id
	}
	return ""
}
