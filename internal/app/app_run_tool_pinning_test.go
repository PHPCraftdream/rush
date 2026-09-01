package app

// Review fix R3-1: ExecuteRun must not publish a per-run toolset onto the
// ONE shared session agent before the ReserveExclusive admission decision.
// A same-session fail-fast LOSER (DisableSubAgents=true) that loses
// admission must therefore be unable to strip the delegation tools from the
// WINNER's (DisableSubAgents=false) live toolset mid-turn: both of the
// winner's provider requests must still carry the delegation tools.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pinnedToolsetProvider(t *testing.T, gate, started chan struct{}, recordedTools *[][]string, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		var names []string
		for _, tool := range req.Tools {
			if tool.Function.Name == "" {
				continue
			}
			names = append(names, tool.Function.Name)
		}
		if !strings.Contains(string(body), `"call_1"`) {
			// FIRST step: record the winner's step-1 tools BEFORE blocking,
			// then hold the winner mid-turn on the gate.
			mu.Lock()
			*recordedTools = append(*recordedTools, names)
			mu.Unlock()
			once.Do(func() { close(started) })
			<-gate
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("c1", "call_1", "bash", `{"command":"echo pin_probe","description":"pin probe"}`),
				admissionSSEStop("c1", "tool_calls"),
			})
			return
		}
		// SECOND step (after the bash tool executed).
		mu.Lock()
		*recordedTools = append(*recordedTools, names)
		mu.Unlock()
		admissionWriteSSE(w, []string{
			admissionSSEText("c2", "PIN_DONE"),
			admissionSSEStop("c2", "stop"),
		})
	}
}

func TestExecuteRunSameSessionBusyLoserCannotClobberWinnerTools(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	var mu sync.Mutex
	var recordedTools [][]string
	application, sessionID := newAdmissionRaceApp(t, pinnedToolsetProvider(t, gate, started, &recordedTools, &mu))

	outcomes := make(chan admissionOutcome, 2)
	start := make(chan struct{})
	// Call 1 — the WINNER: DisableSubAgents=false, so its toolset includes
	// the delegation tools (agent, agentic_fetch).
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "WINNER_TOOLS_MARKER hold", RunOverrides{}, true)
	close(start)

	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("winner never reached the provider; cannot stage the mid-turn winner")
	}

	// Call 2 — the LOSER: DisableSubAgents=true. Pre-fix, it rebuilt and
	// published its (delegation-stripped) toolset onto the shared agent
	// before losing the reservation, clobbering the winner's tools.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "LOSER_TOOLS_MARKER should never reach the provider", RunOverrides{DisableSubAgents: true}, true)
	close(start2)

	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.Error(t, o.err)
		require.ErrorIs(t, o.err, agent.ErrSessionBusy)
		require.Nil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("loser did not fail fast with ErrSessionBusy")
	}

	// Release the winner: it executes the bash tool (auto-approved) and
	// issues its step-2 request.
	close(gate)

	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.NoError(t, o.err)
		require.NotNil(t, o.res)
		require.Equal(t, "end_turn", o.res.ExitReason, "warnings=%v", o.res.Warnings)
	case <-time.After(60 * time.Second):
		t.Fatal("winner did not complete after the gate opened")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, recordedTools, 2, "exactly the winner's two steps may reach the provider; the loser's request must never leave the process")
	for i, names := range recordedTools {
		assert.Contains(t, names, "agent", "step %d: winner's delegation tool 'agent' was clobbered by the rejected loser (R3-1)", i+1)
		assert.Contains(t, names, "agentic_fetch", "step %d: winner's delegation tool 'agentic_fetch' was clobbered by the rejected loser (R3-1)", i+1)
		require.Greater(t, len(names), 3, "step %d: toolset suspiciously small — request-shape parsing regression?", i+1)
	}
	assert.ElementsMatch(t, recordedTools[0], recordedTools[1], "step 2's toolset must be unchanged from step 1's")
}
