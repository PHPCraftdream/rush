// Package B (docs/plans/2026-10-15-role-models-hot-reload.md §3.4/§4): the
// App-owned config disk-sync poller. New starts it right after InitCoderAgent
// so it covers rush (web), rush run, and SDK application mode; poll-detected
// model changes fan out to coalescing subscribers.

package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

// ConfigNotificationsPublished counts the model-change pings that
// publishModelsChanged actually accepted into a subscriber's buffer-1
// channel (a coalesced no-op send is NOT counted — nothing is pending).
// Test-observability hook only: paired with the forwarder-side
// configNotificationsForwarded counter in internal/server/events.go it
// yields the invariant "published == consumed once the forwarder is idle",
// letting the server's negative EventConfig test prove producer-side
// quiescence instead of probing with a timeout.
var ConfigNotificationsPublished atomic.Int64

// modelsRefreshTimeout bounds the background shared-agent rebuild that
// follows a poll-detected model change, so a stuck coordinator cannot pin
// the App's shutdown on stopConfigDiskSync's WaitGroup join.
const modelsRefreshTimeout = 30 * time.Second

// startConfigDiskSync starts the config disk-sync poller under the
// App-owned lifecycle (plan §3.4). Started by New right after InitCoderAgent,
// so it covers rush (web), rush run, and SDK application mode; releaseResources
// stops and joins it before the parallel cleanupFuncs batch.
func (app *App) startConfigDiskSync() {
	if app.config == nil {
		return
	}
	app.config.StartConfigDiskSync(config.ConfigDiskSyncOptions{
		Reload:          app.reloadConfigForSync,
		OnModelsChanged: app.onModelsChangedFromPoll,
	})
}

// reloadConfigForSync is the poller's reload hook: the MCP-aware boundary.
// A nil mcpOwner receiver is safe (ReloadAndReconcileMCPConfig falls back to
// the package-level reload) and routes to the plain reload.
func (app *App) reloadConfigForSync(ctx context.Context) error {
	return app.mcpOwner.ReloadAndReconcileMCPConfig(ctx, app.config)
}

// onModelsChangedFromPoll runs on the poller goroutine, so anything heavier
// than a publish must go async (plan §3.6). The shared default agent is
// refreshed ONLY on a poll-detected model change, never on every reload
// (plan §3.7); the rebuild goroutine is joined by stopConfigDiskSync.
func (app *App) onModelsChangedFromPoll(config.ModelsChange) {
	app.publishModelsChanged()
	if app.AgentCoordinator == nil {
		return
	}
	app.modelsSyncWG.Add(1)
	go func() {
		defer app.modelsSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), modelsRefreshTimeout)
		defer cancel()
		if err := app.AgentCoordinator.UpdateModels(ctx); err != nil {
			slog.Warn("Failed to refresh shared agent models after a config change", "error", err)
		}
	}()
}

// SubscribeModelsChanged returns a per-caller coalescing notification
// channel (plan §4: buffer 1 — a subscriber that has not consumed the last
// ping misses nothing, bursts cannot flood). The channel is REMOVED and
// CLOSED when ctx is cancelled (after removal, under the send lock), so a
// `for range` consumer terminates; double-close is impossible because the
// removal loop runs exactly once per ctx.
func (app *App) SubscribeModelsChanged(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	app.modelsSyncSubsMu.Lock()
	app.modelsSyncSubs = append(app.modelsSyncSubs, ch)
	app.modelsSyncSubsMu.Unlock()
	go func() {
		<-ctx.Done()
		app.modelsSyncSubsMu.Lock()
		for i, sub := range app.modelsSyncSubs {
			if sub == ch {
				app.modelsSyncSubs = append(app.modelsSyncSubs[:i], app.modelsSyncSubs[i+1:]...)
				// Close under the send lock: after removal nothing else
				// holds the channel, so this cannot race a send, and the
				// consumer's `for range` terminates deterministically.
				close(ch)
				break
			}
		}
		app.modelsSyncSubsMu.Unlock()
	}()
	return ch
}

// publishModelsChanged pings every subscriber once; the non-blocking send
// keeps a stalled consumer from blocking the poller goroutine. Sends and
// the unsubscribe close are serialized by the same mutex, so a concurrent
// close can never race a send.
func (app *App) publishModelsChanged() {
	app.modelsSyncSubsMu.Lock()
	defer app.modelsSyncSubsMu.Unlock()
	for _, ch := range app.modelsSyncSubs {
		select {
		case ch <- struct{}{}:
			ConfigNotificationsPublished.Add(1)
		default:
		}
	}
}

// stopConfigDiskSync stops and joins the config disk-sync poller and any
// in-flight shared-agent rebuild. Idempotent; safe on bare App literals.
func (app *App) stopConfigDiskSync() {
	if app.config != nil {
		app.config.StopConfigDiskSync()
	}
	app.modelsSyncWG.Wait()
}
