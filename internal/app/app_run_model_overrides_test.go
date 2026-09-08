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
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type modelOverrideRequest struct {
	Model string `json:"model"`
}

type modelOverrideApp struct {
	app      *App
	store    *config.ConfigStore
	dataDir  string
	requests chan string
	release  chan struct{}
}

func newModelOverrideApp(t *testing.T) modelOverrideApp {
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
		var req modelOverrideRequest
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &req))
		requests <- req.Model
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"override","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"override","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
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
        {"id":"smart-a","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-a","context_window":200000,"default_max_tokens":1000},
        {"id":"smart-b","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-b","context_window":200000,"default_max_tokens":1000},
        {"id":"smart-fail","context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"}
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
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	return modelOverrideApp{
		app:      application,
		store:    store,
		dataDir:  dataDir,
		requests: requests,
		release:  release,
	}
}

func createModelOverrideSession(t *testing.T, application *App, title string) session.Session {
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

func TestExecuteRunModelOverridesArePinnedPerCallAndPersisted(t *testing.T) {
	h := newModelOverrideApp(t)
	sessA := createModelOverrideSession(t, h.app, "override-a")
	sessB := createModelOverrideSession(t, h.app, "override-b")

	type outcome struct {
		result *RunResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, tc := range []struct {
		sessionID, smart, fast string
	}{
		{sessA.ID, "smart-a", "fast-a"},
		{sessB.ID, "smart-b", "fast-b"},
	} {
		wg.Add(1)
		go func(tc struct{ sessionID, smart, fast string }) {
			defer wg.Done()
			result, err := h.app.ExecuteRun(context.Background(), RunRequest{
				Prompt:            "run " + tc.smart,
				ContinueSessionID: tc.sessionID,
				Overrides:         RunOverrides{SmartModel: tc.smart, FastModel: tc.fast},
				Mode:              RunModeJSON,
				Stdout:            io.Discard,
				Stderr:            io.Discard,
				HideSpinner:       true,
				FailIfSessionBusy: true,
			})
			outcomes <- outcome{result: result, err: err}
		}(tc)
	}

	gotModels := map[string]bool{}
	for len(gotModels) < 2 {
		gotModels[<-h.requests] = true
	}
	close(h.release)
	wg.Wait()
	close(outcomes)
	for result := range outcomes {
		require.NoError(t, result.err)
		require.NotNil(t, result.result)
	}

	require.Equal(t, "smart-default", h.store.Config().Models[config.SelectedModelTypeSmart].Model)
	require.Equal(t, "fast-default", h.store.Config().Models[config.SelectedModelTypeFast].Model)
	storedA, err := h.app.Sessions.Get(t.Context(), sessA.ID)
	require.NoError(t, err)
	storedB, err := h.app.Sessions.Get(t.Context(), sessB.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-a", storedA.SmartModelID)
	require.Equal(t, "fast-a", storedA.FastModelID)
	require.Equal(t, "smart-b", storedB.SmartModelID)
	require.Equal(t, "fast-b", storedB.FastModelID)
}

func TestExecuteRunInvalidFastModelDoesNotPersistValidSmartModel(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "invalid-fast")

	_, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "invalid fast",
		ContinueSessionID: sess.ID,
		Overrides:         RunOverrides{SmartModel: "smart-a", FastModel: "missing-fast"},
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		FailIfSessionBusy: true,
	})
	require.Error(t, err)
	require.Empty(t, h.requests)
	after, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelID)
	require.Empty(t, after.FastModelID)
	require.Equal(t, "smart-default", h.store.Config().Models[config.SelectedModelTypeSmart].Model)
	require.Equal(t, "fast-default", h.store.Config().Models[config.SelectedModelTypeFast].Model)
}

func TestExecuteRunModelPersistenceRollbackSkipsProvider(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "transaction-failure")
	_, err := h.app.DB().ExecContext(t.Context(), `
		CREATE TRIGGER fail_model_update
		BEFORE UPDATE OF smart_model_id ON sessions
		WHEN NEW.smart_model_id = 'smart-fail'
		BEGIN SELECT RAISE(ABORT, 'model persistence failure'); END`)
	require.NoError(t, err)

	_, err = h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "transaction failure",
		ContinueSessionID: sess.ID,
		Overrides:         RunOverrides{SmartModel: "smart-fail", FastModel: "fast-a"},
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		FailIfSessionBusy: true,
	})
	require.Error(t, err)
	require.Empty(t, h.requests)
	after, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelID)
	require.Empty(t, after.FastModelID)
}

func TestExecuteRunModelOverridePreservesOmittedSessionSlot(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "preserve-omitted")
	require.NoError(t, h.app.Sessions.UpdateModels(t.Context(), sess.ID, nil,
		&session.ModelSlotUpdate{Provider: "openaicompat", Model: "fast-b"}))

	resultCh := make(chan error, 1)
	go func() {
		_, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "smart only",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		resultCh <- err
	}()
	require.Equal(t, "smart-a", <-h.requests)
	close(h.release)
	require.NoError(t, <-resultCh)

	after, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-a", after.SmartModelID)
	require.Equal(t, "fast-b", after.FastModelID)
}

func TestExecuteRunModelOverrideSurvivesFreshAppContinuation(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "reopen-override")
	resultCh := make(chan error, 1)
	go func() {
		_, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "persist before reopen",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-a", FastModel: "fast-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		resultCh <- err
	}()
	require.Equal(t, "smart-a", <-h.requests)
	close(h.release)
	require.NoError(t, <-resultCh)
	oldDB := h.app.DB()
	h.app.Shutdown()
	freshStore, err := config.Init(h.dataDir, h.dataDir, false)
	require.NoError(t, err)
	require.NotSame(t, h.store, freshStore)
	conn, err := db.Connect(t.Context(), h.dataDir)
	require.NoError(t, err)
	require.NotSame(t, oldDB, conn)
	fresh, err := New(t.Context(), conn, freshStore)
	require.NoError(t, err)
	t.Cleanup(fresh.Shutdown)
	after, err := fresh.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-a", after.SmartModelID)
	require.Equal(t, "fast-a", after.FastModelID)
	continuation, err := fresh.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "continue without flags",
		ContinueSessionID: sess.ID,
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		FailIfSessionBusy: true,
	})
	require.NoError(t, err)
	require.NotNil(t, continuation)
	require.Equal(t, "smart-a", <-h.requests)
}

func TestExecuteRunQueuedOverridePersistsWhenPromoted(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "queued-override")
	outcomes := make(chan struct {
		idx int
		err error
	}, 2)
	go func() {
		_, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "owner",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-a", FastModel: "fast-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		outcomes <- struct {
			idx int
			err error
		}{1, err}
	}()
	require.Equal(t, "smart-a", <-h.requests)

	go func() {
		_, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "queued",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-b", FastModel: "fast-b"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		outcomes <- struct {
			idx int
			err error
		}{2, err}
	}()

	var queued bool
	select {
	case outcome := <-outcomes:
		require.Equal(t, 2, outcome.idx)
		require.ErrorIs(t, outcome.err, ErrRunQueued)
		queued = true
	}
	require.True(t, queued)
	close(h.release)
	require.NoError(t, (<-outcomes).err)
	require.Equal(t, "smart-b", <-h.requests)

	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-b", stored.SmartModelID)
	require.Equal(t, "fast-b", stored.FastModelID)
}

func TestExecuteRunQueuedOverridePersistenceFailureSkipsProviderAndCleansMailbox(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "queued-override-failure")
	_, err := h.app.DB().ExecContext(t.Context(), `
		CREATE TRIGGER fail_queued_model_update
		BEFORE UPDATE OF smart_model_id ON sessions
		WHEN NEW.smart_model_id = 'smart-b'
		BEGIN SELECT RAISE(ABORT, 'queued model persistence failure'); END`)
	require.NoError(t, err)

	outcomes := make(chan error, 2)
	go func() {
		_, runErr := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "owner",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-a", FastModel: "fast-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		outcomes <- runErr
	}()
	require.Equal(t, "smart-a", <-h.requests)
	go func() {
		_, runErr := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "queued",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-b", FastModel: "fast-b"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
		})
		outcomes <- runErr
	}()
	queuedErr := <-outcomes
	require.ErrorIs(t, queuedErr, ErrRunQueued)
	close(h.release)
	_ = <-outcomes

	select {
	case model := <-h.requests:
		t.Fatalf("queued call reached provider after persistence failure: %s", model)
	default:
	}
	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-a", stored.SmartModelID)
	require.Equal(t, "fast-a", stored.FastModelID)
	require.False(t, h.app.AgentCoordinator.IsSessionBusy(sess.ID))
	pending, err := h.app.Sessions.ListPendingRunQueueEntries(t.Context())
	require.NoError(t, err)
	var recovered bool
	for _, entry := range pending {
		if entry.SessionID == sess.ID && strings.Contains(entry.CallData, `"smart-b"`) && strings.Contains(entry.CallData, `"fast-b"`) {
			recovered = true
		}
	}
	require.True(t, recovered, "queued B must remain durably recoverable after admission-time persistence failure")
}

func TestExecuteRunBusyModelOverrideDoesNotMutateSession(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "busy-override")

	firstDone := make(chan error, 1)
	go func() {
		_, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "winner",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-a", FastModel: "fast-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		firstDone <- err
	}()
	require.Equal(t, "smart-a", <-h.requests)

	_, err := h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "loser",
		ContinueSessionID: sess.ID,
		Overrides:         RunOverrides{SmartModel: "smart-b", FastModel: "fast-b"},
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		FailIfSessionBusy: true,
	})
	require.Error(t, err)
	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "smart-a", stored.SmartModelID)
	require.Equal(t, "fast-a", stored.FastModelID)

	close(h.release)
	require.NoError(t, <-firstDone)
}
