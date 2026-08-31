package sdk_test

// R1-1 (P0) + R1-4 (P1) regression tests, end-to-end through sdk.Client.
//
// R1-1: two concurrent runs on ONE Client must each execute under their
// OWN per-call policy (restricted-run allowlist, cost/token caps,
// peak-hours bypass, model role). Before the per-call execution context,
// both runs raced for the coordinator's shared Set*-state and the
// permission service's single process-wide run gate — the restricted
// tenant's tool call could be approved under the unrestricted tenant's
// gate and vice versa, depending on which run armed the shared state
// last. The assertions below check WHICH policy actually applied (real
// tool call, real denial), not just the absence of a data race.
//
// R1-4: two runs started SIMULTANEOUSLY on the same idle session must
// resolve to exactly one owner: the loser fails with agent.ErrSessionBusy
// and is never silently queued behind the winner.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// sseChunks writes an openai-compat SSE response from raw chunk objects.
func sseChunks(t *testing.T, w http.ResponseWriter, chunks []map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, chunk := range chunks {
		b, err := json.Marshal(chunk)
		require.NoError(t, err)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func textChunk(model, text string) map[string]any {
	return map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
}

func finishChunk(model, reason string) map[string]any {
	return map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
		"usage": map[string]any{"prompt_tokens": 17, "completion_tokens": 5, "total_tokens": 22},
	}
}

// toolCallChunk emits one bash tool call for the given command.
func toolCallChunk(model, callID, command string) map[string]any {
	args, err := json.Marshal(map[string]any{"command": command, "description": "policy probe"})
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index":    0,
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": "bash", "arguments": string(args)},
			}}},
			"finish_reason": nil,
		}},
	}
}

// peakWindowForRushJSON returns a peak_hours window guaranteed to cover
// the current wall clock (wide margin around the current hour).
func peakWindowForRushJSON() (start, end string) {
	now := time.Now()
	return now.Add(-2 * time.Hour).Format("15:04"), now.Add(2 * time.Hour).Format("15:04")
}

// writeTwoProviderRushJSON writes a rush.json with two openai-compat
// providers: probe-a (no peak_hours, the workspace default smart/fast)
// and probe-b (peak_hours covering now). Per-session model overrides
// route individual sessions to probe-b.
func writeTwoProviderRushJSON(t *testing.T, workDir, urlA, urlB string) {
	t.Helper()
	start, end := peakWindowForRushJSON()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "probe-a": {
      "id": "probe-a", "name": "probe-a", "type": "openai-compat",
      "base_url": %q, "api_key": "key-a", "discover_models": false,
      "models": [{"id": "probe-a", "name": "probe-a", "context_window": 200000, "default_max_tokens": 1000}]
    },
    "probe-b": {
      "id": "probe-b", "name": "probe-b", "type": "openai-compat",
      "base_url": %q, "api_key": "key-b", "discover_models": false,
      "peak_hours": {"start": %q, "end": %q},
      "models": [{"id": "probe-b", "name": "probe-b", "context_window": 200000, "default_max_tokens": 1000}]
    }
  },
  "models": {
    "smart": {"provider": "probe-a", "model": "probe-a"},
    "fast": {"provider": "probe-a", "model": "probe-a"}
  }
}`, urlA, urlB, start, end)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))
}

// routeSessionToProviderB points sessionID's smart+fast slots at probe-b
// (per-session DB override, same mechanism the web UI's model picker uses).
func routeSessionToProviderB(t *testing.T, workDir, sessionID string) {
	t.Helper()
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	sessions := session.NewService(db.New(conn), conn)
	slot := &session.ModelSlotUpdate{Provider: "probe-b", Model: "probe-b"}
	require.NoError(t, sessions.UpdateModels(context.Background(), sessionID, slot, slot))
}

func toolResultsOf(msgs []sdk.Message) []message.ToolResult {
	var out []message.ToolResult
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		out = append(out, m.ToolResults()...)
	}
	return out
}

// TestSDKConcurrentRunsPerCallPolicyIsolation: two SIMULTANEOUS runs on
// one Client, each with its own session, its own provider, and OPPOSITE
// per-call policies. Tenant A runs restricted (nothing allowlisted) and
// capped; its model issues a bash tool call that MUST be denied. Tenant B
// runs unrestricted with a peak-hours bypass (its provider is inside a
// peak_hours window) and MUST complete cleanly. A's denial proves the
// restricted gate was applied to A even while B's (inert) gate was armed
// on the same process-wide slot concurrently — the exact race the shared
// SetRunAllowlist used to lose.
func TestSDKConcurrentRunsPerCallPolicyIsolation(t *testing.T) {
	isolateGlobalConfig(t)

	const (
		sessionA    = "sdk-policy-a"
		sessionB    = "sdk-policy-b"
		aCommand    = "echo policy-a-ok && echo probe"
		aDeniedOnce = "A_RUN_FINISHED"
		bMarker     = "B_RUN_FINISHED"
	)

	// Provider A: request 1 answers with a bash tool call (a compound
	// command, so it cannot ride the safe-read-only fast path and always
	// reaches the permission gate). Later requests (if the denial ever
	// loops back) get plain text.
	var aReqMu sync.Mutex
	aRequests := 0
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		aReqMu.Lock()
		aRequests++
		n := aRequests
		aReqMu.Unlock()
		if n == 1 {
			sseChunks(t, w, []map[string]any{toolCallChunk("probe-a", "call-1", aCommand), finishChunk("probe-a", "tool_calls")})
			return
		}
		sseChunks(t, w, []map[string]any{textChunk("probe-a", aDeniedOnce), finishChunk("probe-a", "stop")})
	}))
	t.Cleanup(srvA.Close)

	// Provider B: single-round plain text. Its provider config carries a
	// peak_hours window covering now, so only the per-call bypass lets
	// this run through at all.
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sseChunks(t, w, []map[string]any{textChunk("probe-b", bMarker), finishChunk("probe-b", "stop")})
	}))
	t.Cleanup(srvB.Close)

	workDir := t.TempDir()
	writeTwoProviderRushJSON(t, workDir, srvA.URL, srvB.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	seedTenantSession(t, workDir, sessionA)
	seedTenantSession(t, workDir, sessionB)
	routeSessionToProviderB(t, workDir, sessionB)

	var (
		wg      sync.WaitGroup
		startCh = make(chan struct{})
		resB    *sdk.RunResult
		errA    error
		errB    error
		bufA    bytes.Buffer
		bufB    bytes.Buffer
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		_, errA = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "run the bash probe",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionA,
			Stdout:            &bufA,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				RestrictedRun: true, // allow nothing: the bash probe must be denied
				MaxCost:       1.5,
				MaxTokens:     500000,
			},
		})
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resB, errB = client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "reply with exactly the marker text and nothing else",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionB,
			Stdout:            &bufB,
			HideSpinner:       true,
			Overrides: sdk.RunOverrides{
				AllowPeakHours: true, // per-call bypass; provider B is in-window
			},
		})
	}()
	close(startCh)
	wg.Wait()

	// Tenant A's run (whatever its envelope says — a denial can end the
	// turn in-band) must never be mistaken for a busy rejection: A and B
	// ran on DIFFERENT sessions.
	require.NotErrorIs(t, errA, agent.ErrSessionBusy, "tenant A output %q", bufA.String())

	// Tenant B: the per-call peak-hours bypass must hold even while
	// tenant A (no bypass, different provider) ran concurrently.
	require.NoError(t, errB, "tenant B output %q", bufB.String())
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	require.Equal(t, bMarker, resB.FinalText)

	// Tenant A: the restricted-run policy must have applied to A's OWN
	// tool call. The bash probe is the only tool call in the session and
	// its result must be the clean permission denial — not an execution.
	msgsA, err := client.Messages(context.Background(), sessionA)
	require.NoError(t, err)
	results := toolResultsOf(msgsA)
	require.NotEmpty(t, results, "tenant A must have attempted (and had denied) the bash tool call; output %q", bufA.String())
	for _, tr := range results {
		require.Equal(t, "bash", tr.Name)
		require.True(t, tr.IsError, "restricted tenant's tool call must be denied, got content %q", tr.Content)
		require.Contains(t, tr.Content, "User denied permission")
		require.NotContains(t, tr.Content, "policy-a-ok", "the command must NOT have executed under the restricted policy")
	}
	// The command's OUTPUT never reached the session in any role.
	for _, m := range msgsA {
		for _, part := range m.Parts {
			if tc, ok := part.(message.ToolResult); ok {
				require.NotContains(t, tc.Content, "policy-a-ok")
			}
		}
	}
}

// TestSDKRunSameSessionConcurrentStartExactlyOneWins: two runs started
// SIMULTANEOUSLY on the same IDLE session. The mailbox reservation is the
// single atomic decision point, so exactly one run becomes the owner; the
// loser must fail with agent.ErrSessionBusy and must never disappear into
// the silent queue (R1-4 — the pre-fix behavior for a lost check-then-act
// race).
func TestSDKRunSameSessionConcurrentStartExactlyOneWins(t *testing.T) {
	isolateGlobalConfig(t)

	const (
		sessionID   = "sdk-race-same-session"
		firstMarker = "RACE_WINNER_TURN_OK"
	)

	gate := make(chan struct{})
	started := make(chan struct{})
	srv := gatedProviderServer(t, firstMarker, gate, started)

	workDir := t.TempDir()
	writeProbeRushJSON(t, workDir, srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	seedTenantSession(t, workDir, sessionID)

	type runOutcome struct {
		res *sdk.RunResult
		err error
		out bytes.Buffer
	}
	run := func() <-chan runOutcome {
		ch := make(chan runOutcome, 1)
		go func() {
			var buf bytes.Buffer
			res, err := client.Run(context.Background(), sdk.RunRequest{
				Prompt:            "hold the session",
				Mode:              sdk.RunModeJSON,
				ContinueSessionID: sessionID,
				Stdout:            &buf,
				HideSpinner:       true,
			})
			ch <- runOutcome{res: res, err: err, out: buf}
		}()
		return ch
	}

	// Barrier: fire both from idle at the same instant.
	first := run()
	second := run()

	// The first outcome to arrive MUST be the busy rejection: the winner
	// is parked on the provider gate and cannot finish before we open it.
	var loser runOutcome
	select {
	case loser = <-first:
	case loser = <-second:
	case <-time.After(60 * time.Second):
		t.Fatal("neither run returned while both should have resolved quickly (one busy, one gated)")
	}
	require.Error(t, loser.err, "the run that lost the idle-session race must fail")
	require.ErrorIs(t, loser.err, agent.ErrSessionBusy,
		"the loser must wrap agent.ErrSessionBusy, not queue silently")
	if loser.res != nil {
		// The JSON envelope path still returns a summary alongside the
		// error — it must plainly say the turn errored, never succeed.
		require.Equal(t, "error", loser.res.ExitReason, "loser envelope %s", loser.res.Error)
	}

	// Now release the winner and collect its outcome from the other channel.
	close(gate)
	var winner runOutcome
	select {
	case winner = <-first:
	case winner = <-second:
	case <-time.After(60 * time.Second):
		t.Fatal("the winning run never completed after the provider was released")
	}
	require.NoError(t, winner.err, "winner output %q", winner.out.String())
	require.NotNil(t, winner.res)
	require.Equal(t, "end_turn", winner.res.ExitReason)
	require.Equal(t, firstMarker, winner.res.FinalText)
}
