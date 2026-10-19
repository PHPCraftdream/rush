// Package B (docs/plans/2026-10-15-role-models-hot-reload.md §3.4/§4): the
// App-owned config disk-sync poller starts with New and stops with shutdown;
// the models-changed fan-out coalesces and unsubscribes on ctx cancel.

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestAppNewStartsAndShutdownStopsConfigDiskSync(t *testing.T) {
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

	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("copilot", config.ProviderConfig{
		ID:      "copilot",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		OAuthToken: &oauth.Token{
			AccessToken:  "fixture-access",
			RefreshToken: "fixture-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		},
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Runs after the Shutdown cleanup below (LIFO).
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	// Failure-path cleanup; the explicit Shutdown below is idempotent with it.
	t.Cleanup(application.Shutdown)
	require.NotNil(t, application.AgentCoordinator)
	require.True(t, store.ConfigDiskSyncRunning(), "App.New must start the config disk-sync poller after InitCoderAgent")

	result := application.ShutdownWithResult()
	require.False(t, result.Forced)
	require.Empty(t, result.CleanupErrors)
	require.False(t, store.ConfigDiskSyncRunning(), "App shutdown must stop the config disk-sync poller")
}

func TestSubscribeModelsChangedCoalescesAndUnsubscribes(t *testing.T) {
	// Pure App literal: the subscription machinery must not need config,
	// a coordinator, or any other fixture.
	app := &App{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch1 := app.SubscribeModelsChanged(ctx1)
	ch2 := app.SubscribeModelsChanged(ctx2)

	app.publishModelsChanged()
	// Non-blocking receive: each channel holds exactly one ping.
	select {
	case <-ch1:
	default:
		t.Fatal("ch1 must receive the first publish")
	}
	select {
	case <-ch2:
	default:
		t.Fatal("ch2 must receive the first publish")
	}
	// Two more publishes without consumption: buffer 1 coalesces them.
	app.publishModelsChanged()
	app.publishModelsChanged()
	app.publishModelsChanged()
	select {
	case <-ch1:
	default:
		t.Fatal("coalesced ping must be buffered")
	}
	select {
	case <-ch1:
		t.Fatal("coalescing must keep at most one buffered ping")
	default:
	}
	select {
	case <-ch2:
	default:
		t.Fatal("ch2 must still hold the coalesced ping")
	}

	// Cancel ctx1 and wait for the removal-and-close goroutine to run
	// before publishing, so the assertions are deterministic.
	cancel1()
	require.Eventually(t, func() bool {
		app.modelsSyncSubsMu.Lock()
		defer app.modelsSyncSubsMu.Unlock()
		return len(app.modelsSyncSubs) == 1
	}, time.Second, time.Millisecond)

	app.publishModelsChanged()
	select {
	case <-ch2:
	default:
		t.Fatal("ch2 must receive the ping after ch1 unsubscribed")
	}
	// After cancellation the subscription is closed: the receive terminates
	// immediately with ok=false (nothing was buffered after the unsubscribe).
	_, ok := <-ch1
	require.False(t, ok, "the cancelled subscription must be closed")
}

func TestOnModelsChangedPublishesWithoutCoordinator(t *testing.T) {
	// A bare App literal has a nil AgentCoordinator: the callback must
	// still publish and must not panic on the nil guard.
	app := &App{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := app.SubscribeModelsChanged(ctx)
	app.onModelsChangedFromPoll(config.ModelsChange{})
	select {
	case <-ch:
	default:
		t.Fatal("subscriber must receive the ping even with a nil AgentCoordinator")
	}
}

// TestSubscribeModelsChangedClosesOnCancel is P2-1's exit signal: the
// subscription channel is CLOSED once ctx cancellation removes it, so a
// `for range` consumer (the server's EventConfig forwarder) terminates
// without needing its own ctx branch. Revert-check: removing the close in
// SubscribeModelsChanged's removal goroutine leaves the channel open
// forever and this receive never reports ok=false.
func TestSubscribeModelsChangedClosesOnCancel(t *testing.T) {
	app := &App{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := app.SubscribeModelsChanged(ctx)
	cancel()
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ch:
			return !ok
		default:
			return false
		}
	}, time.Second, time.Millisecond, "the subscription channel must be closed after ctx cancellation")
}

// TestAppShutdownJoinsConfigDiskSyncBeforeCleanupBatch (P2-3.6): shutdown
// must JOIN the config disk-sync poller (stopConfigDiskSync runs before the
// parallel cleanupFuncs batch, internal/app/app_lifecycle.go:339-344) before
// any cleanup func runs — the batch is where mcpOwner.Close lives
// (internal/app/app.go:491-495, app_lifecycle.go:347-368), and a poll-driven
// reload must never race a closing MCP owner.
//
// Revert-check: moving the stopConfigDiskSync call AFTER the cleanup batch
// (or removing it) leaves the blocked poller join out of the critical path —
// the cleanup batch starts during the 500ms window below and the first
// t.Fatal fires.
func TestAppShutdownJoinsConfigDiskSyncBeforeCleanupBatch(t *testing.T) {
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not reached", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("copilot", config.ProviderConfig{
		ID:      "copilot",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		OAuthToken: &oauth.Token{
			AccessToken:  "fixture-access",
			RefreshToken: "fixture-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		},
		Models: []catwalk.Model{{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000}},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "copilot", Model: "probe"})
	store.SetupAgents()

	// Pre-start the poller with a BLOCKING Reload BEFORE New. StartConfigDiskSync
	// is idempotent (internal/config/store_config_disk_sync.go:85-87: a second
	// start returns early on configSyncStarted), so app.New's own start no-ops
	// and the App keeps OUR stub as the reload hook.
	reloadStarted := make(chan struct{})
	var reloadOnce sync.Once
	releaseReload := make(chan struct{})
	store.StartConfigDiskSync(config.ConfigDiskSyncOptions{
		Reload: func(context.Context) error {
			reloadOnce.Do(func() { close(reloadStarted) })
			<-releaseReload
			return nil
		},
	})

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	// Failure-path cleanup; ShutdownWithResult below is idempotent with it.
	t.Cleanup(application.Shutdown)
	var releaseReloadOnce sync.Once
	closeReload := func() { releaseReloadOnce.Do(func() { close(releaseReload) }) }
	// Registered after Shutdown, so it runs first: a failed ordering check
	// must not leave Shutdown joined on a blocked poller.
	t.Cleanup(closeReload)

	// Trigger a tick: a foreign process changes the global config behind the
	// App's back; the poller's first tick (random stagger, at most 5s apart)
	// hits the digest change and blocks inside the stub Reload.
	foreign, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	require.NoError(t, foreign.UpdatePreferredModels(config.ScopeGlobal, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "copilot", Model: "probe"},
	}))
	require.Eventually(t, func() bool {
		select {
		case <-reloadStarted:
			return true
		default:
			return false
		}
	}, 15*time.Second, 10*time.Millisecond, "the poller's first tick must reach the blocking stub reload")

	// Ordering recorder: an in-package cleanup func that signals when the
	// parallel cleanup batch starts.
	batchStarted := make(chan struct{})
	var batchOnce sync.Once
	application.cleanupFuncs = append(application.cleanupFuncs, func(context.Context) error {
		batchOnce.Do(func() { close(batchStarted) })
		return nil
	})

	done := make(chan struct{})
	go func() {
		application.ShutdownWithResult()
		close(done)
	}()
	// While the poller is blocked in the stub, shutdown must be stuck in the
	// join: neither the cleanup batch nor completion may fire.
	select {
	case <-batchStarted:
		t.Fatal("the cleanup batch started before the config poller was joined")
	case <-done:
		t.Fatal("shutdown returned before the config poller was joined")
	case <-time.After(500 * time.Millisecond):
	}

	closeReload()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete after the poller was released")
	}
	// After done, the batch has already run: a non-blocking check suffices.
	select {
	case <-batchStarted:
	default:
		t.Fatal("the cleanup batch must run after the poller is joined")
	}
}
