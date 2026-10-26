package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const pcfChildSystemMarker = "Given the user's prompt"

type pcfFixtureErrors struct {
	mu       sync.Mutex
	messages []string
}

func (e *pcfFixtureErrors) pcfRecord(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.messages = append(e.messages, s)
}

func (e *pcfFixtureErrors) pcfAll() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.messages...)
}

type pcfCostRouter struct {
	mu        sync.Mutex
	delegated bool
	childLS   bool
	last      map[string]string
}

func pcfNewCostRouter() *pcfCostRouter {
	return &pcfCostRouter{last: make(map[string]string)}
}

func pcfLastMessage(body []byte) (wireMessage, bool) {
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return wireMessage{}, false
	}
	return req.Messages[len(req.Messages)-1], true
}

func pcfStarted(text string) bool {
	return strings.HasPrefix(text, "batch root ") && strings.Contains(text, "started; completions arrive as selected notices.")
}

func pcfCompletion(text, wanted string) bool {
	rest, ok := strings.CutPrefix(text, "Async job ")
	if !ok {
		return false
	}
	id, tail, ok := strings.Cut(rest, " () ")
	if !ok || id != wanted {
		return false
	}
	for _, suffix := range []string{"finished.", "failed.", "was stopped (batch_stop).", "was cancelled (session stopped).", "was interrupted:", "timed out after"} {
		if strings.HasPrefix(tail, suffix) {
			return true
		}
	}
	return false
}

func pcfContinuation(text string) bool {
	return text == "Continue based on the notice(s) above." ||
		text == "The conversation was summarized; continue reacting to the notice(s) above." ||
		strings.HasPrefix(text, "<system_reminder>")
}

func (r *pcfCostRouter) pcfRoute(body []byte) (string, error) {
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return "", fmt.Errorf("malformed provider request")
	}
	system := ""
	for _, m := range req.Messages {
		if m.Role == "system" {
			s, _ := m.Content.(string)
			if strings.Contains(s, "short title") {
				return "title", nil
			}
			system = s
		}
	}
	actor := "root"
	if strings.Contains(system, pcfChildSystemMarker) {
		actor = "child"
	}
	m, _ := pcfLastMessage(body)
	text, _ := m.Content.(string)
	r.mu.Lock()
	defer r.mu.Unlock()
	if m.Role == "user" && pcfContinuation(text) {
		last := r.last[actor]
		if last == "" || last == "root:delegate" || last == "child:bash" {
			return "", fmt.Errorf("invalid continuation")
		}
		return last, nil
	}
	var phase string
	switch {
	case actor == "root" && ((m.Role == "tool" && m.ToolCallID == "call-agent") || m.Role == "user") && pcfStarted(text):
		if !r.delegated {
			return "", fmt.Errorf("root started before delegation")
		}
		phase = "root:yield"
	case actor == "root" && m.Role == "user" && pcfCompletion(text, "call-agent"):
		if !r.delegated {
			return "", fmt.Errorf("root completion before delegation")
		}
		phase = "root:final"
	case actor == "root" && m.Role == "user" && strings.HasPrefix(text, "Async job "):
		return "", fmt.Errorf("unexpected root completion")
	case actor == "root" && m.Role == "assistant" && strings.Contains(text, "root waiting"):
		phase = "root:yield"
	case actor == "root" && m.Role == "assistant" && strings.Contains(text, "R5-ROOT-DONE"):
		phase = "root:final"
	case actor == "root" && m.Role == "user":
		if r.delegated {
			return "", fmt.Errorf("root delegated more than once")
		}
		r.delegated = true
		phase = "root:delegate"
	case actor == "child" && m.Role == "user" && strings.Contains(text, "WORKER-MARKER"):
		if r.childLS {
			return "", fmt.Errorf("child first turn repeated")
		}
		r.childLS = true
		phase = "child:bash"
	case actor == "child" && m.Role == "tool" && m.ToolCallID == "call-ls" && pcfStarted(text):
		if !r.childLS {
			return "", fmt.Errorf("child yield before first turn")
		}
		phase = "child:yield"
	case actor == "child" && m.Role == "user" && pcfStarted(text):
		if !r.childLS {
			return "", fmt.Errorf("child yield before first turn")
		}
		phase = "child:yield"
	case actor == "child" && m.Role == "tool" && m.ToolCallID == "call-ls":
		if !r.childLS {
			return "", fmt.Errorf("child ls result before first turn")
		}
		phase = "child:blocked"
	case actor == "child" && m.Role == "user" && pcfCompletion(text, "call-ls"):
		if !r.childLS {
			return "", fmt.Errorf("child completion before first turn")
		}
		phase = "child:blocked"
	case actor == "child" && m.Role == "user" && pcfCompletion(text, "call-agent"):
		return "", fmt.Errorf("unexpected child completion")
	case actor == "child" && m.Role == "assistant" && strings.Contains(text, "child done"):
		if !r.childLS {
			return "", fmt.Errorf("child done before first turn")
		}
		phase = "child:done"
	default:
		return "", fmt.Errorf("unexpected %s request", actor)
	}
	r.last[actor] = phase
	return phase, nil
}

func TestPcfCostRouterCurrentTail(t *testing.T) {
	pcfBody := func(ms ...wireMessage) []byte {
		t.Helper()
		b, err := json.Marshal(wireRequest{Messages: ms})
		require.NoError(t, err)
		return b
	}
	pcfRoot := wireMessage{Role: "system", Content: "coder"}
	pcfChild := wireMessage{Role: "system", Content: "Given the user's prompt"}
	pcfUser := func(s string) wireMessage { return wireMessage{Role: "user", Content: s} }
	pcfTool := func(id, s string) wireMessage { return wireMessage{Role: "tool", ToolCallID: id, Content: s} }
	pcfStartedText := "batch root abc started; completions arrive as selected notices."
	for _, pcfEnd := range []string{"finished.", "failed.", "was stopped (batch_stop).", "was cancelled (session stopped).", "was interrupted: reason", "timed out after 1s"} {
		pcfR := pcfNewCostRouter()
		pcfRoute, pcfErr := pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
		require.Equal(t, "root:delegate", pcfRoute)
		require.Empty(t, pcfErr)
		pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfTool("call-agent", pcfStartedText)))
		require.Equal(t, "root:yield", pcfRoute)
		require.Empty(t, pcfErr)
		pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser(pcfStartedText)))
		require.Equal(t, "root:yield", pcfRoute)
		require.NoError(t, pcfErr)
		pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("Async job call-agent () "+pcfEnd)))
		require.Equal(t, "root:final", pcfRoute)
		require.Empty(t, pcfErr)
	}
	pcfR := pcfNewCostRouter()
	_, pcfErr := pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
	require.Empty(t, pcfErr)
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
	require.Error(t, pcfErr)
	pcfR = pcfNewCostRouter()
	pcfRoute, pcfErr := pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("WORKER-MARKER")))
	require.Equal(t, "child:bash", pcfRoute)
	require.Empty(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfTool("call-ls", "inline ls output")))
	require.Equal(t, "child:blocked", pcfRoute)
	require.Empty(t, pcfErr)
	pcfR = pcfNewCostRouter()
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
	require.NoError(t, pcfErr)
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("Async job wrong () finished.")))
	require.Error(t, pcfErr)
	pcfR = pcfNewCostRouter()
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("WORKER-MARKER")))
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfTool("call-ls", "batch root abc started; completions arrive as selected notices.")))
	require.Equal(t, "child:yield", pcfRoute)
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("batch root abc started; completions arrive as selected notices.")))
	require.Equal(t, "child:yield", pcfRoute)
	require.NoError(t, pcfErr)
	pcfR = pcfNewCostRouter()
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("WORKER-MARKER")))
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("Async job call-ls () finished.")))
	require.Equal(t, "child:blocked", pcfRoute)
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfChild, pcfUser("Async job call-agent () finished.")))
	require.Empty(t, pcfRoute)
	require.Error(t, pcfErr)
	pcfR = pcfNewCostRouter()
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
	require.NoError(t, pcfErr)
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfTool("call-agent", pcfStartedText)))
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("Continue based on the notice(s) above.")))
	require.Equal(t, "root:yield", pcfRoute)
	require.NoError(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, wireMessage{Role: "user", Content: "<system_reminder>todo list</system_reminder>"}))
	require.Equal(t, "root:yield", pcfRoute)
	require.NoError(t, pcfErr)
	pcfR = pcfNewCostRouter()
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("start")))
	require.NoError(t, pcfErr)
	_, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("Continue based on the notice(s) above.")))
	require.Error(t, pcfErr)
	pcfRoute, pcfErr = pcfR.pcfRoute(pcfBody(pcfRoot, pcfUser("unknown")))
	require.Empty(t, pcfRoute)
	require.Error(t, pcfErr)
	_, pcfErr = pcfNewCostRouter().pcfRoute([]byte("not json"))
	require.Error(t, pcfErr)
	pcfRoute, pcfErr = pcfNewCostRouter().pcfRoute(pcfBody(pcfRoot, pcfUser("Async job call-agent () finished.")))
	require.Empty(t, pcfRoute)
	require.Error(t, pcfErr)
}
