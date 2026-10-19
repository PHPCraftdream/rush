// Package B (docs/plans/2026-10-15-role-models-hot-reload.md §2.4): an E2E
// check that a model change written behind the App's back is adopted by the
// App's OWN config poller and re-broadcast to WebSocket clients as a full
// config event, and that unrelated config writes are not. All assertions are
// taken from the subscription side; a broadcast marker frame received after
// registration proves the client is registered on the hub (client delivery
// ordering) — the model-change subscription itself is registered
// synchronously before subscribeAndBroadcast returns, so no race remains by
// construction (no sleeps, no polling guesses).

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
	// the client proves the CLIENT is registered on the hub (client
	// delivery ordering). The model-change subscription is registered
	// synchronously inside subscribeAndBroadcast before it returns, so no
	// model-subscription race remains by construction; the marker makes no
	// claim about forwarder-goroutine readiness (it needn't: a publish
	// landing before the first receive is buffered in the channel).
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

	// Negative case, baseline→settler→count. An unrelated config write must
	// not fire OnModelsChanged and must not produce a new EventConfig frame.
	// We do NOT race the poller:
	//  1. drain everything buffered now (frames of the earlier positive change);
	//  2. the unrelated write (options.debug), waited out until adopted;
	//  3. producer-side quiescence barrier: published == consumed proves the
	//     forwarder has consumed EVERY notification ever accepted into the
	//     buffer — nothing is pending/unconsumed before the settler write;
	//  4. a SECOND known-positive model change (probe-b → probe, a real
	//     models diff that fires OnModelsChanged) publishes a settler ping.
	//     We wait until BOTH counters advance: published advancing proves
	//     the settler's ping was ACCEPTED into the buffer (not coalesced
	//     away behind an unconsumed ping), consumed advancing proves the
	//     forwarder picked it up and broadcast the frame. FIFO proof: hub
	//     fan-out appends to the client's buffered channel in broadcast
	//     order (hub.go:497-513), so anything enqueued BEFORE the settler
	//     is already in client.send when the settler is received.
	//  5. the first EventConfig after the drain must BE the settler (a
	//     mutant debug-reload frame would arrive first and carry probe-b),
	//     and no further EventConfig may follow it.
	countConfigEvents(client.send) // drain: frames of the positive change above are not under test
	require.NoError(t, foreign.SetConfigFields(config.ScopeGlobal, map[string]any{"options.debug": true}))
	require.Eventually(t, func() bool {
		o := application.Store().Config().Options
		return o != nil && o.Debug
	}, 20*time.Second, 10*time.Millisecond, "the poller must adopt the unrelated write before the negative window opens")

	// Producer-side quiescence barrier (deterministic, no time probe):
	// published == consumed means the forwarder is idle — every accepted
	// notification has been consumed and broadcast, and none is pending.
	// REVERT-CHECK (every-reload mutant, fires OnModelsChanged on the debug
	// reload): its debug ping is published either
	//   (a) while the barrier is polling → published == consumed+1 until
	//       consumed; if it stays buffered when the settler ping is sent,
	//       the settler's send is coalesced away, published NEVER advances,
	//       and the published-and-consumed advance assertion below times
	//       out — the merge escape is closed at the producer; or
	//   (b) already consumed → its frame is enqueued FIFO BEFORE the
	//       settler's, and the first EventConfig below decodes "probe-b"
	//       and fails the settler-model assertion.
	// Under correct code no debug ping is ever published and the barrier is
	// trivially satisfied (both counters froze after the positive phase).
	require.Eventually(t, func() bool {
		return appPkg.ConfigNotificationsPublished.Load() == configNotificationsForwarded.Load()
	}, 5*time.Second, 5*time.Millisecond,
		"the forwarder must be idle (published == consumed) before the settler write: a still-buffered debug notification would coalesce the settler ping away")

	// Settler: a second, known-positive models change orders the negative
	// window behind a delivered frame. Capture BOTH counters BEFORE the
	// settler write, then wait for both to advance: published advancing
	// rules out coalescing (the ping was accepted), consumed advancing
	// proves the frame was broadcast and enqueued on the hub before we
	// block on the client channel.
	pubBefore := appPkg.ConfigNotificationsPublished.Load()
	beforeSettler := configNotificationsForwarded.Load()
	require.NoError(t, foreign.UpdatePreferredModels(config.ScopeGlobal, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "copilot", Model: "probe"},
	}))
	require.Eventually(t, func() bool {
		return appPkg.ConfigNotificationsPublished.Load() > pubBefore &&
			configNotificationsForwarded.Load() > beforeSettler
	}, 12*time.Second, 10*time.Millisecond,
		"the settler's notification must be accepted AND consumed; a stalled published counter means the settler ping was coalesced away behind an unconsumed debug ping")
	settler := waitForEvent(t, client, EventConfig, 12*time.Second)
	require.NotNil(t, settler, "the second models change must deliver a settler EventConfig")
	var settlerWire ConfigWire
	require.NoError(t, json.Unmarshal(settler.Payload, &settlerWire))
	require.Equal(t, "probe", settlerWire.Models["smart"].Model,
		"the settler frame must carry the reverted smart model")

	// waitForEvent returned the FIRST EventConfig after the drain: a mutant
	// frame from the debug reload (carrying probe-b) would have arrived first and
	// failed the settler-model assertion above. Nothing may follow the settler
	// either.
	after := countConfigEvents(client.send)
	require.Zero(t, after,
		"no EventConfig may follow the settler; an extra frame means an unrelated reload broadcast")
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
