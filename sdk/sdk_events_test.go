package sdk_test

// Live structured-event subscription tests for sdk.Client.SubscribeMessages
// and sdk.Client.SubscribeSessions. Both are thin pass-throughs to the same
// pubsub brokers that feed the web UI (internal/server/events.go): a
// subscription opened BEFORE a Run must observe the run's session and
// message lifecycle events through the raw Go types —
// pubsub.Event[message.Message] / pubsub.Event[session.Session] — with no
// WS-specific reshaping in between.
//
// Same isolation pattern as
// TestOpenRunsWithWorkingDirDifferentFromProcessCwd (sdk_test.go):
// throwaway global config/data dirs via env, an httptest.NewServer speaking
// openai-compatible SSE, and a rush.json written into the temp working
// directory. The run is a single provider round with no tool calls; the
// background title-generation call (needsTitle fires for an implicitly
// created session, internal/agent/agent_turn.go) is served by its own
// marker branch so it can never be confused with the main flow.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

const (
	eventsFinalText    = "EVENTS_PASS"
	eventsDrainTimeout = 10 * time.Second
)

// newEventsTestEnv performs the sdk_test.go isolation dance and returns a
// client whose provider is the mock SSE server below.
func newEventsTestEnv(t *testing.T) (client *sdk.Client, stdout, stderr *bytes.Buffer) {
	t.Helper()

	// Isolate global config/data resolution (same env set as sdk_test.go)
	// so config.Init inside sdk.Open reads only the rush.json written
	// below into the temp working directory — never the operator's real
	// global config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	workDir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
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
		if bytes.Contains(body, []byte("Generate a concise title for")) {
			// Background title generation (fires for an implicitly
			// created session): an unrelated call this test does not
			// assert on — give it a clean one-chunk stop so it never
			// falls into the main-flow branch below.
			write([]string{
				`{"id":"ct","object":"chat.completion.chunk","created":9,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"Test Title"},"finish_reason":null}]}`,
				`{"id":"ct","object":"chat.completion.chunk","created":9,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			})
			return
		}
		// Main flow: one provider round, no tool calls, final text.
		write([]string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, eventsFinalText),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":4,"total_tokens":17}}`,
		})
	}))
	t.Cleanup(srv.Close)

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
}`, srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cl, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: workDir,
		Stdout:     out,
		Stderr:     errOut,
	})
	require.NoError(t, err)
	require.NotNil(t, cl)
	t.Cleanup(func() { _ = cl.Close() })

	return cl, out, errOut
}

// collectUntil drains ch until pred holds over the accumulated events or
// the timeout expires. It never blocks forever: a missing event fails the
// caller's assertions with what DID arrive instead of hanging the test.
func collectUntil[T any](ch <-chan T, timeout time.Duration, pred func(got []T) bool) []T {
	var got []T
	deadline := time.After(timeout)
	for {
		if pred(got) {
			return got
		}
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
}

func summarizeMessageEvents(evs []sdk.MessageEvent, sessionID string) (created, updated, fromSession int) {
	for _, ev := range evs {
		switch ev.Type {
		case pubsub.CreatedEvent:
			created++
		case pubsub.UpdatedEvent:
			updated++
		}
		if ev.Payload.SessionID == sessionID {
			fromSession++
		}
	}
	return created, updated, fromSession
}

func describeMessageEvents(evs []sdk.MessageEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("{%s session=%s role=%s}", ev.Type, ev.Payload.SessionID, ev.Payload.Role))
	}
	return out
}

func describeSessionEvents(evs []sdk.SessionEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("{%s id=%s title=%q}", ev.Type, ev.Payload.ID, ev.Payload.Title))
	}
	return out
}

func TestSubscribeMessagesReceivesLiveEvents(t *testing.T) {
	cl, _, _ := newEventsTestEnv(t)

	// Subscribe BEFORE the run: ExecuteRun publishes through the same
	// broker regardless of subscribers, so this ordering proves events
	// arrive live rather than being replayed after the fact.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	msgCh := cl.SubscribeMessages(ctx)

	// JSON mode so the envelope (and its SessionID) comes back; no
	// ContinueSessionID — the app creates the session implicitly.
	res, err := cl.Run(context.Background(), sdk.RunRequest{
		Prompt:      "say the magic words",
		Mode:        sdk.RunModeJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "run must finish cleanly; res.Error=%q", res.Error)
	require.Equal(t, eventsFinalText, res.FinalText)

	// Expected for a one-turn answer: CreatedEvents (the user and
	// assistant message creates — internal/message Create publishes
	// CreatedEvent) and at least one UpdatedEvent (the assistant's
	// terminal write is a publish-must-deliver Update).
	events := collectUntil(msgCh, eventsDrainTimeout, func(got []sdk.MessageEvent) bool {
		created, updated, fromSession := summarizeMessageEvents(got, res.SessionID)
		return created >= 1 && updated >= 1 && fromSession >= 1
	})
	created, updated, fromSession := summarizeMessageEvents(events, res.SessionID)
	require.GreaterOrEqual(t, created, 1,
		"expected at least one message CreatedEvent; got %v", describeMessageEvents(events))
	require.GreaterOrEqual(t, updated, 1,
		"expected at least one message UpdatedEvent (assistant terminal write); got %v", describeMessageEvents(events))
	require.GreaterOrEqual(t, fromSession, 1,
		"expected at least one event for session %q; got %v", res.SessionID, describeMessageEvents(events))
}

func TestSubscribeSessionsReceivesLiveEvents(t *testing.T) {
	cl, _, _ := newEventsTestEnv(t)

	// Subscribe BEFORE the run — see TestSubscribeMessagesReceivesLiveEvents.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sessCh := cl.SubscribeSessions(ctx)

	// Run without ContinueSessionID: resolveSession's default branch
	// creates the session through app.Sessions.Create — the publish site
	// of the session CreatedEvent (internal/session session lifecycle).
	res, err := cl.Run(context.Background(), sdk.RunRequest{
		Prompt:      "answer briefly",
		Mode:        sdk.RunModeJSON,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "run must finish cleanly; res.Error=%q", res.Error)

	events := collectUntil(sessCh, eventsDrainTimeout, func(got []sdk.SessionEvent) bool {
		for _, ev := range got {
			if ev.Type == pubsub.CreatedEvent && ev.Payload.ID == res.SessionID {
				return true
			}
		}
		return false
	})
	var sawCreated bool
	for _, ev := range events {
		if ev.Type == pubsub.CreatedEvent && ev.Payload.ID == res.SessionID {
			sawCreated = true
			break
		}
	}
	require.True(t, sawCreated,
		"expected a session CreatedEvent for %q; got %v", res.SessionID, describeSessionEvents(events))
}
