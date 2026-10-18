// Package B (docs/plans/2026-10-15-role-models-hot-reload.md §2.4): an E2E
// check that a model change written behind the App's back is adopted by the
// App's OWN config poller and re-broadcast to WebSocket clients as a full
// config event, and that unrelated config writes are not. All assertions are
// taken from the subscription side; a broadcast marker frame received after
// registration acts as a readiness barrier (no sleeps, no polling guesses).

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSubscribeAndBroadcastForwardsModelsChangedAsEventConfig(t *testing.T) {
	// Isolate BOTH global config paths (CLAUDE.md: RUSH_GLOBAL_DATA and
	// RUSH_GLOBAL_CONFIG are separate resolutions) before app.New reads
	// the host config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// Provider-only unsigned workDir/rush.json (same provider shape as the
	// sdk OAuth lifecycle test): a copilot provider with two probe models.
	// The smart/fast SELECTION is in-memory only, via SetSelectedModelRuntime,
	// like the internal/app lifecycle test. The file must carry NO models
	// block: project files outrank global data in the config merge (plan
	// §1.1), so a project models block would shadow the foreign global-data
	// write and OnModelsChanged could never fire.
	workDir := t.TempDir()
	dataDir := t.TempDir()
	content := fmt.Sprintf(`{"disable_default_providers": true, "providers": {"copilot": {"id": "copilot", "name": "copilot", "type": "openai-compat", "base_url": %q, "api_key": "probe", "discover_models": false, "models": [{"id": "probe", "name": "probe", "context_window": 200000, "default_max_tokens": 1000}, {"id": "probe-b", "name": "probe-b", "context_window": 200000, "default_max_tokens": 1000}]}}}`, srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(content), 0o644))

	store, err := config.Init(workDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseAll(dataDir) })

	application, err := appPkg.New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := newHub()
	go hub.Run(ctx)
	client := newClient(hub, nil)
	client.send = make(chan []byte, 100)
	hub.register <- client
	subscribeAndBroadcast(ctx, application, hub)

	// Registration barrier: a directly broadcast marker frame received by
	// the client proves registration AND that the forwarder goroutines are
	// running — no sleep, no readiness guesswork.
	hub.Broadcast(EventSessionUpdated, session.Session{ID: "barrier"})
	waitForEvent(t, client, EventSessionUpdated, 3*time.Second)

	// A foreign store sharing the same isolated dirs writes a new smart
	// model behind the App's back; the App's OWN poller adopts it (random
	// first delay in [0,5s), hence the generous deadline) and the reload's
	// models diff fans out exactly one coalescing EventConfig.
	foreign, err := config.Init(workDir, dataDir, false)
	require.NoError(t, err)
	require.NoError(t, foreign.UpdatePreferredModels(config.ScopeGlobal, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "copilot", Model: "probe-b"},
	}))

	frame := waitForEvent(t, client, EventConfig, 12*time.Second)
	require.NotNil(t, frame, "the poll-detected model change must broadcast an EventConfig frame")
	var wire ConfigWire
	require.NoError(t, json.Unmarshal(frame.Payload, &wire))
	require.Equal(t, "probe-b", wire.Models["smart"].Model,
		"the EventConfig snapshot must carry the foreign-written smart model")
	require.Eventually(t, func() bool {
		return application.Store().Config().Models[config.SelectedModelTypeSmart].Model == "probe-b"
	}, 5*time.Second, 10*time.Millisecond, "the poll-driven reload must adopt the foreign-written smart model")

	// Negative case: an unrelated config write must not fire
	// OnModelsChanged and must not produce a new EventConfig frame. Drain
	// everything buffered first (duplicate frames from the positive window
	// are legal — the background poller may also have adopted the write).
	_ = countConfigEvents(client.send)
	require.NoError(t, foreign.SetConfigFields(config.ScopeGlobal, map[string]any{"options.debug": true}))
	newConfigs := countConfigEventsWithin(t, client, 2*time.Second)
	require.Zero(t, newConfigs,
		"an unrelated config write must not produce a new EventConfig frame")
}

// waitForEvent drains the client channel until one frame of the wanted type
// arrives or the deadline passes; the frame is returned decoded, nil on
// timeout.
func waitForEvent(t *testing.T, client *Client, eventType string, within time.Duration) *WSMessage {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			if json.Unmarshal(raw, &env) == nil && env.Type == eventType {
				return &env
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// countConfigEvents drains whatever is currently buffered on the client
// channel and counts EventConfig frames.
func countConfigEvents(send <-chan []byte) int {
	count := 0
	for {
		select {
		case raw := <-send:
			var env WSMessage
			if json.Unmarshal(raw, &env) == nil && env.Type == EventConfig {
				count++
			}
		default:
			return count
		}
	}
}

// countConfigEventsWithin counts EventConfig frames arriving within the
// window; used for the negative assertion, where "no frame EVER arrives" is
// the property under test regardless of poller timing.
func countConfigEventsWithin(t *testing.T, client *Client, within time.Duration) int {
	t.Helper()
	count := 0
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			if json.Unmarshal(raw, &env) == nil && env.Type == EventConfig {
				count++
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	return count
}
