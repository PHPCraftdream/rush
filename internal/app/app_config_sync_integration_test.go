// P2-3 (docs/plans/2026-10-15-role-models-hot-reload.md:555-596): the
// role-models hot-reload integration tests, driven through the PRODUCTION
// entry point App.ExecuteRun. A foreign process's ConfigStore (a second
// config.Init over the same isolated dirs) writes rush.json behind the
// App's back; the App-owned config disk-sync poller must adopt the change
// and the next turn must run on the reloaded model.
//
// Scope note: the foreign writes below use config.ScopeWorkspace because
// configPath(ScopeWorkspace) resolves to the store's workspacePath —
// dataDir/rush.json for a store inited with (dataDir, dataDir) (internal/
// config/load.go:93-96, internal/config/store_write.go:22-33) — which is
// exactly the project file this fixture seeds, the highest-priority source
// of the models block.
//
// The explicit store.PollConfigDiskOnce shortcut is deliberately NOT used
// to force adoption: it would keep the tests green even if app.New stopped
// calling startConfigDiskSync (internal/app/app.go:584). The second turn of
// TestRoleModelHotReload_NextTurnUsesNewSmart instead relies on the
// background poller alone, behind a generous channel deadline.
package app

import (
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

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type roleModelSyncRequest struct {
	Model string `json:"model"`
}

type roleModelSyncApp struct {
	app   *App
	store *config.ConfigStore
	// foreign is a SECOND config.Init over the same isolated dirs, created
	// AFTER app.New: a foreign process's store whose SetConfigFields /
	// UpdatePreferredModels writes the same files behind the App's back.
	foreign  *config.ConfigStore
	dataDir  string
	requests chan string
	release  chan struct{}
	// closeRelease opens the SSE gate once; also registered as the first
	// cleanup so a failed test never leaves a handler parked on it.
	closeRelease func()
}

func newRoleModelSyncApp(t *testing.T) roleModelSyncApp {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	requests := make(chan string, 8)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req roleModelSyncRequest
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &req))
		requests <- req.Model
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"role-sync","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"role-sync","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":"smart-x","context_window":200000,"default_max_tokens":1000},
        {"id":"smart-2","context_window":200000,"default_max_tokens":1000},
        {"id":"worker-default","context_window":200000,"default_max_tokens":1000},
        {"id":"worker-2","context_window":200000,"default_max_tokens":1000},
        {"id":"reviewer-default","context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"},
    "worker": {"provider":"openaicompat","model":"worker-default"},
    "reviewer": {"provider":"openaicompat","model":"reviewer-default"}
  }
}`, srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = fmt.Errorf("%w (db release: %v)", err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	// The foreign store: a second process's view of the same dirs. Created
	// AFTER app.New so its snapshot predates none of the App's own state.
	foreign, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)

	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered last, so it runs FIRST: handlers parked on the gate are
	// released before the App shuts down and the server closes.
	t.Cleanup(closeRelease)

	return roleModelSyncApp{
		app:          application,
		store:        store,
		foreign:      foreign,
		dataDir:      dataDir,
		requests:     requests,
		release:      release,
		closeRelease: closeRelease,
	}
}

func createRoleSyncSession(t *testing.T, application *App, title string) session.Session {
	t.Helper()
	sess, err := application.Sessions.Create(t.Context(), title)
	require.NoError(t, err)
	_, err = application.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)
	return sess
}

// runOutcome carries a background ExecuteRun's result back to the test
// goroutine, so require assertions run on the test goroutine.
type runOutcome struct {
	result *RunResult
	err    error
}

// waitForModelRequest reads the next recorded model within the deadline.
func waitForModelRequest(t *testing.T, requests <-chan string, within time.Duration) string {
	t.Helper()
	select {
	case model := <-requests:
		return model
	case <-time.After(within):
		t.Fatalf("no provider request arrived within %s", within)
		return ""
	}
}

// TestRoleModelHotReload_NextTurnUsesNewSmart: a foreign workspace write to
// the smart slot is adopted by the App-owned background poller, and the
// next turn runs on the reloaded model — with NO explicit
// PollConfigDiskOnce call (see the package header for why).
func TestRoleModelHotReload_NextTurnUsesNewSmart(t *testing.T) {
	h := newRoleModelSyncApp(t)
	sess := createRoleSyncSession(t, h.app, "hot-reload-smart")

	turn1 := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "turn one",
			ContinueSessionID: sess.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		turn1 <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-default", waitForModelRequest(t, h.requests, 12*time.Second))
	h.closeRelease()
	outcome := <-turn1
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)

	// The foreign process rewrites the project rush.json behind the App's
	// back; the App-owned poller must adopt it on its own.
	require.NoError(t, h.foreign.UpdatePreferredModels(config.ScopeWorkspace, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "openaicompat", Model: "smart-2"},
	}))
	require.Eventually(t, func() bool {
		return h.store.Config().Models[config.SelectedModelTypeSmart].Model == "smart-2"
	}, 20*time.Second, 10*time.Millisecond, "the background poller must adopt the foreign smart-slot write")

	turn2 := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "turn two",
			ContinueSessionID: sess.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		turn2 <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-2", waitForModelRequest(t, h.requests, 12*time.Second),
		"the next turn must run on the reloaded smart model")
	// release is already closed, so the SSE handler unblocks immediately.
	outcome = <-turn2
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)
}

// TestRoleModelHotReload_ExplicitSessionChoiceSurvives: an explicit
// session-level smart choice (plan rule 3) keeps precedence over a
// reloaded config for the pinned session, while fresh sessions pick up the
// reloaded slot.
func TestRoleModelHotReload_ExplicitSessionChoiceSurvives(t *testing.T) {
	h := newRoleModelSyncApp(t)
	sessB := createRoleSyncSession(t, h.app, "pinned-smart")
	require.NoError(t, h.app.Sessions.UpdateModels(t.Context(), sessB.ID,
		&session.ModelSlotUpdate{Provider: "openaicompat", Model: "smart-x"}, nil))

	pinned := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "pinned turn",
			ContinueSessionID: sessB.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		pinned <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-x", waitForModelRequest(t, h.requests, 12*time.Second),
		"the session-pinned smart model must win over config")
	h.closeRelease()
	outcome := <-pinned
	require.NoError(t, outcome.err)

	require.NoError(t, h.foreign.UpdatePreferredModels(config.ScopeWorkspace, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "openaicompat", Model: "smart-2"},
	}))
	require.Eventually(t, func() bool {
		return h.store.Config().Models[config.SelectedModelTypeSmart].Model == "smart-2"
	}, 20*time.Second, 10*time.Millisecond, "the background poller must adopt the foreign smart-slot write")

	sessC := createRoleSyncSession(t, h.app, "fresh-after-reload")
	fresh := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "fresh turn",
			ContinueSessionID: sessC.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		fresh <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-2", waitForModelRequest(t, h.requests, 12*time.Second),
		"a fresh session must run on the reloaded smart model")
	outcome = <-fresh
	require.NoError(t, outcome.err)
}

// TestRoleModelHotReload_InFlightKeepsPinnedModel: a config adoption that
// publishes WHILE a turn is in flight must not disturb the in-flight call;
// the recorded first-request model is captured by the fixture before the
// SSE handler blocks, so this asserts clean completion plus turn 2 running
// on the new model (resolving shared state mid-turn would swap the
// in-flight model).
func TestRoleModelHotReload_InFlightKeepsPinnedModel(t *testing.T) {
	h := newRoleModelSyncApp(t)
	sess := createRoleSyncSession(t, h.app, "in-flight-pin")

	turn1 := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "in-flight turn",
			ContinueSessionID: sess.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		turn1 <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-default", waitForModelRequest(t, h.requests, 12*time.Second))

	// The reload publishes while turn 1 is blocked mid-response.
	require.NoError(t, h.foreign.UpdatePreferredModels(config.ScopeWorkspace, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeSmart: {Provider: "openaicompat", Model: "smart-2"},
	}))
	require.Eventually(t, func() bool {
		return h.store.Config().Models[config.SelectedModelTypeSmart].Model == "smart-2"
	}, 20*time.Second, 10*time.Millisecond, "the background poller must adopt the foreign smart-slot write mid-turn")

	h.closeRelease()
	outcome := <-turn1
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result)

	turn2 := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "after-reload turn",
			ContinueSessionID: sess.ID,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		turn2 <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-2", waitForModelRequest(t, h.requests, 12*time.Second),
		"the turn after the in-flight one must run on the reloaded smart model")
	outcome = <-turn2
	require.NoError(t, outcome.err)
}

// TestRoleModelHotReload_WorkerRoleLiveAndNotPersisted (P2-6 + plan test 4):
// a worker-role run resolves the live worker slot via the shared
// roleSlotOverride cascade, rides the explicit --effort for THIS invocation
// only, and persists neither the role model nor the effort onto the
// session's smart/fast fields.
func TestRoleModelHotReload_WorkerRoleLiveAndNotPersisted(t *testing.T) {
	h := newRoleModelSyncApp(t)
	sess := createRoleSyncSession(t, h.app, "worker-role-live")

	workerTurn := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "worker turn",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeWorker, ReasoningEffort: "high"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		workerTurn <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "worker-default", waitForModelRequest(t, h.requests, 12*time.Second),
		"the worker role must resolve the live worker slot, not the smart one")
	h.closeRelease()
	outcome := <-workerTurn
	require.NoError(t, outcome.err)

	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SmartModelID, "a role-derived model must not be persisted")
	require.Empty(t, stored.FastModelReasoningEffort, "the explicit effort must not be persisted for a worker run")

	// A foreign write to the worker slot is adopted live; the next worker
	// run picks it up, while a smart-role run stays on the smart slot.
	require.NoError(t, h.foreign.UpdatePreferredModels(config.ScopeWorkspace, map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeWorker: {Provider: "openaicompat", Model: "worker-2"},
	}))
	require.Eventually(t, func() bool {
		return h.store.Config().Models[config.SelectedModelTypeWorker].Model == "worker-2"
	}, 20*time.Second, 10*time.Millisecond, "the background poller must adopt the foreign worker-slot write")

	workerSess := createRoleSyncSession(t, h.app, "worker-after-reload")
	workerOut := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "worker turn two",
			ContinueSessionID: workerSess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeWorker, ReasoningEffort: "high"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		workerOut <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "worker-2", waitForModelRequest(t, h.requests, 12*time.Second),
		"the next worker run must use the reloaded worker slot")
	outcome = <-workerOut
	require.NoError(t, outcome.err)

	smartSess := createRoleSyncSession(t, h.app, "smart-role-live")
	smartOut := make(chan runOutcome, 1)
	go func() {
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "smart role turn",
			ContinueSessionID: smartSess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		smartOut <- runOutcome{result: result, err: err}
	}()
	require.Equal(t, "smart-default", waitForModelRequest(t, h.requests, 12*time.Second),
		"a smart-role run must stay on the smart slot")
	outcome = <-smartOut
	require.NoError(t, outcome.err)
}
