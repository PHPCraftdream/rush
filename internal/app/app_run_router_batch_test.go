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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
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
	// last maps actor -> the last phase ROUTED successfully for that actor,
	// so a retried request tailing with the synthetic Drain nudge can be
	// served as a continuation of that phase without re-validating it.
	last map[string]string
}

func newRBRouter() *rbRouter {
	return &rbRouter{seen: make(map[string]int), last: make(map[string]string)}
}

// pr2DrainNudge reports whether text is one of production's NON-PERSISTED
// synthetic user tails: the Drain nudges (internal/agent/agent_turn.go, the
// IsDrain && Prompt == "" path) and the todo reminder that preparePrompt
// appends in a <system_reminder> wrapper (internal/agent/agent_prompt.go,
// recognised in production by isTodoReminder, agent_drain.go). A retry that
// follows a dangling assistant turn ends its provider request with one of
// these user messages instead of the phase's own tail text, so the router
// must treat it as a continuation of the actor's last routed phase, never as
// a new delegation.
func pr2DrainNudge(text string) bool {
	return text == "Continue based on the notice(s) above." ||
		text == "The conversation was summarized; continue reacting to the notice(s) above." ||
		strings.HasPrefix(text, "<system_reminder>")
}

// pr2Excerpt bounds a diagnostic to one line of at most 80 characters, so a
// rejection message identifies the offending request without dumping the
// whole prompt body.
func pr2Excerpt(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\n", " "), "\r", " ")
	if len(text) > 80 {
		return text[:80] + "..."
	}
	return text
}

// pr2AwaitShellTerminal polls the batch projection until the shell node of
// the command root owned by sessionID reaches a terminal state, so a
// fixture goroutine can observe the command's REAL completion while a held
// provider response is still blocked (a deterministic mid-turn gate, no
// sleeps on the completion path). It returns an error naming the last
// observed state once the wait elapses.
func pr2AwaitShellTerminal(app *App, sessionID, rootID string, wait time.Duration) error {
	terminal := map[string]bool{
		"completed": true, "failed": true, "cancelled": true,
		"timed_out": true, "interrupted": true, "skipped": true,
	}
	deadline := time.Now().Add(wait)
	last := "no row observed"
	for time.Now().Before(deadline) {
		var state string
		err := app.DB().QueryRowContext(context.Background(),
			`SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`,
			sessionID, rootID).Scan(&state)
		switch {
		case err == nil && terminal[state]:
			return nil
		case err == nil:
			last = state
		default:
			last = err.Error()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("command root never reached a terminal state within %s (last: %s)", wait, last)
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
		case pr2DrainNudge(text):
			// A retry whose tail is the synthetic Drain nudge continues the
			// actor's LAST routed phase: serve it again WITHOUT re-validation
			// (that phase already passed sequence validation) and without
			// mutating the seen counters. Historical events are never replayed.
			r.mu.Lock()
			last, ok := r.last[actor]
			r.mu.Unlock()
			if !ok {
				return actor + ":continuation", "continuation nudge with no prior " + actor + " phase"
			}
			return last, ""
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
			r.mu.Lock()
			reissued := false
			for key := range r.seen {
				if strings.HasPrefix(key, "root|") {
					reissued = true
					break
				}
			}
			r.mu.Unlock()
			if reissued {
				return "root:delegate", "unexpected root user last message after routed root phases (not a fresh delegation): " + pr2Excerpt(text)
			}
			route = "root:delegate"
		default:
			seqErr = "unexpected user last message actor=" + actor + " text=" + pr2Excerpt(text)
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
			seqErr = "unexpected tool last message call=" + callID + " text=" + pr2Excerpt(text)
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
			seqErr = "unexpected assistant last message text=" + pr2Excerpt(text)
		}
	default:
		seqErr = "unexpected last message role=" + lastRole
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if seqErr == "" {
		r.last[actor] = route
	}
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
