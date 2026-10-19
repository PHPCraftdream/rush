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
// Test-observability hook only: counts accepted buffer-1 sends; it does not
// guarantee that a consumer received or processed the notification.
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
	app.modelsSyncSubsMu.Lock()
	if app.modelsSyncClosed {
		app.modelsSyncSubsMu.Unlock()
		return
	}
	app.modelsSyncWG.Add(1)
	app.modelsSyncSubsMu.Unlock()
	go func() {
		defer app.modelsSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), modelsRefreshTimeout)
		defer cancel()
		if err := app.AgentCoordinator.UpdateModels(ctx); err != nil {
			slog.Warn("Failed to refresh shared agent models after a config change", "error", err)
		}
	}()
}

// SubscribeModelsChanged returns a buffered, coalescing notification channel.
// The channel closes on caller cancellation or App shutdown; shutdown joins
// the subscription workers.
func (app *App) SubscribeModelsChanged(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	app.modelsSyncSubsMu.Lock()
	if app.modelsSyncClosed {
		close(ch)
		app.modelsSyncSubsMu.Unlock()
		return ch
	}
	if app.modelsSyncStop == nil {
		app.modelsSyncStop = make(chan struct{})
	}
	app.modelsSyncSubs = append(app.modelsSyncSubs, ch)
	app.modelsSyncSubsWG.Add(1)
	app.modelsSyncSubsMu.Unlock()
	go func() {
		defer app.modelsSyncSubsWG.Done()
		select {
		case <-ctx.Done():
		case <-app.modelsSyncStop:
		}
		app.modelsSyncSubsMu.Lock()
		for i, sub := range app.modelsSyncSubs {
			if sub == ch {
				app.modelsSyncSubs = append(app.modelsSyncSubs[:i], app.modelsSyncSubs[i+1:]...)
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

// stopConfigDiskSync stops and joins the poller, subscribers, and model
// refresh workers. Idempotent; safe on bare App literals.
func (app *App) stopConfigDiskSync() {
	if app.config != nil {
		app.config.StopConfigDiskSync()
	}
	app.modelsSyncSubsMu.Lock()
	if !app.modelsSyncClosed {
		app.modelsSyncClosed = true
		if app.modelsSyncStop != nil {
			close(app.modelsSyncStop)
		}
		for _, ch := range app.modelsSyncSubs {
			close(ch)
		}
		app.modelsSyncSubs = nil
	}
	app.modelsSyncSubsMu.Unlock()
	app.modelsSyncSubsWG.Wait()
	app.modelsSyncWG.Wait()
}
