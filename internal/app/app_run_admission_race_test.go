package app

// Round-2 SDK review R2-1/R2-3 admission-race barrier tests: two
// simultaneous FailIfSessionBusy calls on ONE session. The loser must be
// rejected BEFORE any per-run mutation, and after its cleanup the winner's
// gated tool request must still be judged by the WINNER's own policy —
// plus a regression pin that legacy FailIfSessionBusy==false callers keep
// silently queueing behind the owner.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// newAdmissionRaceApp isolates global config, starts a mock openaicompat
// SSE provider driven by handler, and builds a full App with one seeded
// session (non-default title plus one pre-existing user message, so
// background title generation never fires and every provider request in
// the test belongs to the main flow). Returns the app and the session id.
func newAdmissionRaceApp(t *testing.T, handler http.HandlerFunc) (*App, string) {
	t.Helper()

	// Both GlobalConfig() and GlobalConfigData() resolution paths must be
	// isolated separately (see the golden envelope test's identical block).
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	sess, err := application.Sessions.Create(context.Background(), "admission-race-title")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	return application, sess.ID
}

func admissionWriteSSE(w http.ResponseWriter, chunks []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func admissionSSEText(id, text string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, id, text)
}

func admissionSSEStop(id, finish string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`, id, finish)
}

func admissionSSEToolCall(id, callID, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, callID, name, args)
}

// gatedBashRoundProvider: the first chat request (the winner's turn,
// whichever call it belongs to) closes started and then blocks on gate.
// Once released it emits exactly one bash tool call for a command that is
// in NO allowlist and NOT in the safe-command list, so it must reach the
// restricted-run permission gate. The follow-up round (recognized by the
// tool-call id in the request body) ends the turn with final text.
func gatedBashRoundProvider(t *testing.T, gate, started chan struct{}) http.HandlerFunc {
	t.Helper()
	var once sync.Once
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"call_1"`) {
			once.Do(func() { close(started) })
			<-gate
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("c1", "call_1", "bash", `{"command":"probe_gate_cmd_not_safe","description":"probe gate"}`),
				admissionSSEStop("c1", "tool_calls"),
			})
			return
		}
		admissionWriteSSE(w, []string{
			admissionSSEText("c2", "PROBE_DONE"),
			admissionSSEStop("c2", "stop"),
		})
	}
}

type admissionOutcome struct {
	idx int
	res *RunResult
	err error
}

func admissionLaunch(t *testing.T, application *App, sessionID string, outcomes chan<- admissionOutcome, start <-chan struct{}, idx int, prompt string, overrides RunOverrides, failIfBusy bool) {
	t.Helper()
	go func() {
		<-start
		res, err := application.ExecuteRun(context.Background(), RunRequest{
			Prompt:            prompt,
			Overrides:         overrides,
			Mode:              RunModeJSON,
			ContinueSessionID: sessionID,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: failIfBusy,
		})
		outcomes <- admissionOutcome{idx: idx, res: res, err: err}
	}()
}

// Test A (review matrix row 1): same session, opposite allowlists,
// simultaneous start. The loser returns busy; after the loser's cleanup
// completed, the winner's gated bash request must still be judged by the
// WINNER's own policy — not the loser's, not a fallback.
func TestExecuteRunSameSessionBusyLoserCannotClobberWinnerPolicy(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	application, sessionID := newAdmissionRaceApp(t, gatedBashRoundProvider(t, gate, started))

	// Collect DECIDED permission notifications (Granted or Denied set).
	decided := make(chan permission.PermissionNotification, 16)
	notifCtx, cancelNotif := context.WithCancel(context.Background())
	defer cancelNotif()
	go func() {
		for ev := range application.Permissions.SubscribeNotifications(notifCtx) {
			n := ev.Payload
			if n.Granted || n.Denied {
				decided <- n
			}
		}
	}()

	outcomes := make(chan admissionOutcome, 2)
	start := make(chan struct{})
	// Call 1: restricted, allow-list that does NOT cover the probe command.
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "restricted candidate", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"echo *"},
	}, true)
	// Call 2: unrestricted — its (inert) policy would ALLOW the probe.
	admissionLaunch(t, application, sessionID, outcomes, start, 2, "unrestricted candidate", RunOverrides{}, true)
	close(start)

	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("no run ever reached the provider; cannot stage the mid-turn winner")
	}

	// Exactly one call loses admission and must fail fast with
	// agent.ErrSessionBusy while the winner is still blocked mid-turn.
	var loserIdx int
	select {
	case o := <-outcomes:
		require.Error(t, o.err, "call %d succeeded while the provider gate was still closed", o.idx)
		require.ErrorIs(t, o.err, agent.ErrSessionBusy, "the loser must fail fast with ErrSessionBusy")
		require.Nil(t, o.res)
		loserIdx = o.idx
	case <-time.After(60 * time.Second):
		t.Fatal("no call returned ErrSessionBusy — admission did not fail fast")
	}
	winnerRestricted := loserIdx == 2 // the unrestricted call lost => the winner is the restricted one

	// The loser's ExecuteRun has fully returned, so its cleanup defers have
	// run. Only NOW is the winner's gated bash request judged.
	close(gate)

	select {
	case n := <-decided:
		if winnerRestricted {
			require.True(t, n.Denied, "winner ran RESTRICTED: the bash probe must be DENIED by the winner's own policy (got granted — the winner's policy was lost or fell back, R2-1)")
		} else {
			require.True(t, n.Granted, "winner ran UNRESTRICTED: the bash probe must be GRANTED by the winner's inert policy (got denied — the winner was judged by the loser's policy)")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the winner's gated bash request never produced a permission decision")
	}

	select {
	case o := <-outcomes:
		require.NotEqual(t, loserIdx, o.idx, "the remaining outcome must be the winner's")
		require.NoError(t, o.err, "winner's run failed")
		require.NotNil(t, o.res)
		require.Equal(t, "end_turn", o.res.ExitReason, "warnings=%v", o.res.Warnings)
		if winnerRestricted {
			// A denied bash call stops the turn cleanly WITHOUT executing
			// (the denial response sets StopTurn): the winner's final
			// message has tool-call parts but no text, so FinalText is
			// empty — and that emptiness IS the security assertion that
			// the probe command never ran.
			require.Equal(t, "", o.res.FinalText, "denied probe must not have executed; the turn must have stopped at the denial")
		} else {
			require.Equal(t, "PROBE_DONE", o.res.FinalText)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("winner did not complete after the gate opened")
	}
}

// Test B (review matrix row 2): the busy loser must not change the
// winner's session metadata — system prompt, reasoning effort, budget,
// timeout, ended reason, title, message count — or its permission policy.
func TestExecuteRunSameSessionBusyLoserMutatesNothing(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	application, sessionID := newAdmissionRaceApp(t, gatedBashRoundProvider(t, gate, started))

	winnerOverrides := RunOverrides{
		SystemPrompt:    "WINNER_SYSTEM_PROMPT",
		ReasoningEffort: "low",
		RoleSmart:       true,
		MaxCost:         12.5,
		MaxTokens:       1234,
		Timeout:         111 * time.Second,
		RestrictedRun:   true,
		AllowBash:       []string{"echo *"},
	}
	loserOverrides := RunOverrides{
		SystemPrompt:    "LOSER_SYSTEM_PROMPT_XYZ",
		ReasoningEffort: "high",
		RoleSmart:       true,
		MaxCost:         99.9,
		MaxTokens:       9999,
		Timeout:         999 * time.Second,
		RestrictedRun:   true,
		AllowBash:       []string{"git push"},
	}

	outcomes := make(chan admissionOutcome, 2)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "winner prompt", winnerOverrides, true)
	close(start)

	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("winner never reached the provider; cannot stage the mid-turn winner")
	}

	before, err := application.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)

	// Fire the loser only once the winner is provably mid-turn (all of the
	// winner's mutations already landed — they precede the turn goroutine).
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "loser prompt with different metadata", loserOverrides, true)
	close(start2)

	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.Error(t, o.err)
		require.ErrorIs(t, o.err, agent.ErrSessionBusy)
		require.Nil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("loser neither failed nor returned")
	}

	after, err := application.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID)
	require.Equal(t, before.SystemPrompt, after.SystemPrompt, "loser must not rewrite the system prompt")
	require.Equal(t, before.SmartModelReasoningEffort, after.SmartModelReasoningEffort, "loser must not rewrite reasoning effort")
	require.Equal(t, before.BudgetMaxCost, after.BudgetMaxCost, "loser must not rewrite the cost budget")
	require.Equal(t, before.BudgetMaxTokens, after.BudgetMaxTokens, "loser must not rewrite the token budget")
	require.Equal(t, before.BudgetTimeoutSec, after.BudgetTimeoutSec, "loser must not rewrite the timeout")
	require.Equal(t, before.EndedReason, after.EndedReason, "loser must not rewrite ended_reason")
	require.Equal(t, before.Title, after.Title, "loser must not rename the session")
	require.Equal(t, before.MessageCount, after.MessageCount, "loser must not add a user message")

	// R2-1: the winner's per-session policy must still be armed after the
	// loser's cleanup ran. Probe the gate directly: the winner is
	// restricted with only "echo *" allowed, so the probe must be denied.
	allowed, err := application.Permissions.Request(context.Background(), permission.CreatePermissionRequest{
		SessionID: sessionID,
		ToolName:  "bash",
		Action:    "execute",
		Params:    tools.BashPermissionsParams{Command: "probe_gate_cmd_not_safe", Description: "policy probe"},
	})
	require.NoError(t, err)
	require.False(t, allowed, "winner's restricted policy must still judge this session after the loser's cleanup (R2-1)")

	// The winner completes cleanly once released.
	close(gate)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.NoError(t, o.err)
		require.NotNil(t, o.res)
		require.Equal(t, "end_turn", o.res.ExitReason)
		// The winner is restricted and the probe command is not allowed, so
		// the in-turn bash call is denied with StopTurn: the turn stops at
		// the denial and nothing executes (empty FinalText is the proof).
		require.Equal(t, "", o.res.FinalText, "denied probe must not have executed; the turn stops at the denial")
	case <-time.After(60 * time.Second):
		t.Fatal("winner did not complete after the gate opened")
	}
}

// Regression pin for the legacy queueing contract (R1-4): with
// FailIfSessionBusy == false a second call on a busy session must still be
// silently QUEUED behind the owner and then executed with its own prompt —
// never rejected with ErrSessionBusy, never lost.
func TestExecuteRunSameSessionLegacyQueueingStillQueuesBehindOwner(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	var orderMu sync.Mutex
	var order []string
	record := func(which string) {
		orderMu.Lock()
		order = append(order, which)
		orderMu.Unlock()
	}
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "LOSER_QUEUE_MARKER"):
			record("loser")
			admissionWriteSSE(w, []string{admissionSSEText("c2", "LOSER_DONE"), admissionSSEStop("c2", "stop")})
		case strings.Contains(string(body), "WINNER_QUEUE_MARKER"):
			record("winner")
			once.Do(func() { close(started) })
			<-gate
			admissionWriteSSE(w, []string{admissionSSEText("c1", "WINNER_DONE"), admissionSSEStop("c1", "stop")})
		default:
			record("other")
			admissionWriteSSE(w, []string{admissionSSEText("c0", "IGNORED"), admissionSSEStop("c0", "stop")})
		}
	})

	outcomes := make(chan admissionOutcome, 2)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "first turn WINNER_QUEUE_MARKER hold", RunOverrides{}, false)
	close(start)

	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("winner never reached the provider")
	}

	// Legacy path: no reservation, no fail-fast — the loser must QUEUE.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "second turn LOSER_QUEUE_MARKER queued", RunOverrides{}, false)
	close(start2)

	// Release the winner; its end-of-turn drain must pick the queued call
	// up and run it as the next turn in the same Run loop.
	close(gate)

	got := 0
	winnerDone, loserDone := false, false
	for got < 2 {
		select {
		case o := <-outcomes:
			// THE queueing assertion: neither call may be rejected with
			// ErrSessionBusy. The queueing caller returns as soon as its
			// call is queued (sessionAgent.Run returns nil, nil), so nil
			// error is expected for BOTH, and res may carry whatever the
			// session-wide message stream had seen by queue time — its
			// FinalText is deliberately NOT asserted.
			require.NoError(t, o.err, "legacy queueing call %d must not fail", o.idx)
			require.NotErrorIs(t, o.err, agent.ErrSessionBusy)
			require.NotNil(t, o.res)
			switch o.idx {
			case 1:
				winnerDone = true
			case 2:
				loserDone = true
			}
			got++
		case <-time.After(120 * time.Second):
			t.Fatalf("runs did not complete (winnerDone=%v loserDone=%v)", winnerDone, loserDone)
		}
	}
	require.True(t, winnerDone, "winner's outcome missing")
	require.True(t, loserDone, "loser's outcome missing")

	// The queued turn must have GENUINELY EXECUTED with its own prompt: the
	// provider saw it strictly after the winner's turn, and the session
	// history ends with the loser's user prompt and its own assistant
	// answer.
	orderMu.Lock()
	require.GreaterOrEqual(t, len(order), 2, "provider saw too few requests: %v", order)
	require.Equal(t, "winner", order[0], "the winner's turn must hit the provider first")
	require.Equal(t, "loser", order[1], "the loser's queued turn must run right after the winner's, in the same Run loop")
	orderMu.Unlock()

	msgs, err := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, err)
	var loserUser, loserAssistant bool
	for _, m := range msgs {
		text := m.FullText()
		if m.Role == message.User && strings.Contains(text, "LOSER_QUEUE_MARKER") {
			loserUser = true
		}
		if m.Role == message.Assistant && strings.Contains(text, "LOSER_DONE") {
			loserAssistant = true
		}
	}
	require.True(t, loserUser, "the queued call's user message must exist in the session history")
	require.True(t, loserAssistant, "the queued call's own assistant answer (LOSER_DONE) must exist in the session history")
}
