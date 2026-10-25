package sdk_test

// Fail-fast busy-session semantics for sdk.Client.Run/RunWithCredentials.
// Both SDK entry points set RunRequest.FailIfSessionBusy, so a second
// concurrent run targeting the SAME ContinueSessionID must return an
// error wrapping agent.ErrSessionBusy while the first turn is still
// in-flight — instead of silently queueing behind it the way `rush run`
// and the web server do. Concurrent runs on DIFFERENT sessions must keep
// succeeding (#814's multi-tenant contract).

import (
	"bytes"
	"context"
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
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// isolateGlobalConfig points the global config/data resolution at a
// throwaway directory for one test (same env set as the other sdk tests),
// so config.Init inside sdk.Open reads only the rush.json written into
// the temp working directory — never the operator's real global config.
func isolateGlobalConfig(t *testing.T) {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
}

// writeProbeRushJSON writes a rush.json selecting an openai-compat
// "probe" provider served by baseURL as both smart and fast, with default
// providers disabled.
func writeProbeRushJSON(t *testing.T, workDir, baseURL string) {
	t.Helper()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "probe": {
      "id": "probe",
      "name": "probe",
      "type": "openai-compat",
      "base_url": %q,
      "api_key": "probe",
      "discover_models": false,
      "models": [
        {"id": "probe", "name": "probe", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "probe", "model": "probe"},
    "fast": {"provider": "probe", "model": "probe"}
  }
}`, baseURL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))
}

// gatedProviderServer answers every chat request with a single-round,
// tool-free assistant message carrying finalText — but only after the
// test closes gate. The first request to arrive closes started, so the
// test can hold the session's owner mid-turn deterministically: while the
// provider request is blocked, the mailbox reservation is held, i.e.
// IsSessionBusy(sessionID) is true.
func gatedProviderServer(t *testing.T, finalText string, gate, started chan struct{}) *httptest.Server {
	t.Helper()
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		<-gate
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(chunks []string) {
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
		write([]string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, finalText),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":5,"total_tokens":22}}`,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSDKRunSameSessionSecondCallFailsFast(t *testing.T) {
	isolateGlobalConfig(t)

	const (
		sessionID   = "sdk-busy-same-session"
		firstMarker = "FIRST_TURN_OK"
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

	// Pre-existing user message: background title generation never fires,
	// so the gated provider serves exactly the main flow.
	seedTenantSession(t, workDir, sessionID)

	type runOutcome struct {
		res *sdk.RunResult
		err error
		out bytes.Buffer
	}
	run := func(prompt string) <-chan runOutcome {
		ch := make(chan runOutcome, 1)
		go func() {
			var buf bytes.Buffer
			res, err := client.Run(context.Background(), sdk.RunRequest{
				Prompt:            prompt,
				Mode:              sdk.RunModeJSON,
				ContinueSessionID: sessionID,
				Stdout:            &buf,
				HideSpinner:       true,
			})
			ch <- runOutcome{res: res, err: err, out: buf}
		}()
		return ch
	}

	// Turn 1: hold the session's owner mid-turn (provider blocked).
	first := run("first turn; hold the session")
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("first run never reached the provider; cannot stage the busy state")
	}

	// Turn 2, same session, while turn 1 is provably in-flight: must fail
	// fast with agent.ErrSessionBusy, NOT queue behind turn 1. The timeout
	// turns a "silently queued" regression into a clear failure instead of
	// a deadlock on the gate below.
	second := run("second turn while the first holds the session")
	var busy runOutcome
	select {
	case busy = <-second:
	case <-time.After(60 * time.Second):
		t.Fatal("second run on a busy session neither failed nor returned — it was silently queued (FailIfSessionBusy not honoured)")
	}
	require.Error(t, busy.err, "second concurrent run on the same session must fail")
	require.ErrorIs(t, busy.err, agent.ErrSessionBusy,
		"the failure must wrap agent.ErrSessionBusy so errors.Is-based classification (sessionBusyGuidance, orchestrators) keeps working")
	require.Nil(t, busy.res)

	// Ordering proof: turn 2's error arrived while turn 1 was still
	// blocked on the provider — the gate is only released now.
	close(gate)
	var done runOutcome
	select {
	case done = <-first:
	case <-time.After(60 * time.Second):
		t.Fatal("first run did not complete after the provider was released")
	}
	require.NoError(t, done.err, "first run output %q", done.out.String())
	require.NotNil(t, done.res)
	require.Equal(t, "end_turn", done.res.ExitReason, "warnings=%v", done.res.Warnings)
	require.Equal(t, firstMarker, done.res.FinalText)
}

func TestSDKConcurrentRunsDifferentSessionsBothSucceed(t *testing.T) {
	isolateGlobalConfig(t)

	const (
		sessionA = "sdk-busy-diff-a"
		sessionB = "sdk-busy-diff-b"
		marker   = "DIFF_SESSIONS_BOTH_OK"
	)

	// Immediate responder: no gating, each turn completes on its own.
	// Reuses the credentials test's server helper (same package).
	server := newCredentialServer(t, marker)

	workDir := t.TempDir()
	writeProbeRushJSON(t, workDir, server.srv.URL)

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	seedTenantSession(t, workDir, sessionA)
	seedTenantSession(t, workDir, sessionB)

	run := func(sessionID string) (*sdk.RunResult, error) {
		var buf bytes.Buffer
		res, err := client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "reply with exactly the marker text and nothing else",
			Mode:              sdk.RunModeJSON,
			ContinueSessionID: sessionID,
			Stdout:            &buf,
			HideSpinner:       true,
		})
		if err != nil {
			return nil, fmt.Errorf("session %s run failed: %w (output %q)", sessionID, err, buf.String())
		}
		if res == nil {
			return nil, fmt.Errorf("session %s: nil envelope", sessionID)
		}
		if res.ExitReason != "end_turn" {
			return res, fmt.Errorf("session %s: exit_reason=%q error=%q warnings=%v output=%q", sessionID, res.ExitReason, res.Error, res.Warnings, buf.String())
		}
		return res, nil
	}

	// Fire both runs SIMULTANEOUSLY on the ONE client, different sessions.
	var (
		wg         sync.WaitGroup
		startCh    = make(chan struct{})
		resA, resB *sdk.RunResult
		errA, errB error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startCh
		resA, errA = run(sessionA)
	}()
	go func() {
		defer wg.Done()
		<-startCh
		resB, errB = run(sessionB)
	}()
	close(startCh)
	wg.Wait()

	require.NoError(t, errA, "concurrent run on session %s", sessionA)
	require.NoError(t, errB, "concurrent run on session %s", sessionB)
	require.Equal(t, marker, resA.FinalText)
	require.Equal(t, marker, resB.FinalText)
}
