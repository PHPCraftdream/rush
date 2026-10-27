package app

import (
	"context"
	"encoding/json"
	"errors"
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

type modelOverrideRequest struct {
	Model  string `json:"model"`
	Effort string `json:"reasoning_effort"`
}

type modelOverrideApp struct {
	app      *App
	store    *config.ConfigStore
	dataDir  string
	requests chan string
	bodies   chan []byte
	release  chan struct{}
}

func waitModelOverrideRequest(t *testing.T, requests <-chan string) string {
	t.Helper()
	select {
	case model := <-requests:
		return model
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for provider request")
		return ""
	}
}

func waitModelOverrideResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run result")
		return nil
	}
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
	bodies := make(chan []byte, 32)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req modelOverrideRequest
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &req))
		requests <- req.Model
		bodies <- body
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
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
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000,"can_reason":true,"reasoning_levels":["low","medium","high"],"default_reasoning_effort":"medium"},
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
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default", ReasoningEffort: "medium"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	return modelOverrideApp{
		app:      application,
		store:    store,
		dataDir:  dataDir,
		requests: requests,
		bodies:   bodies,
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

func TestExecuteRunStoredModelAndOverrideArePerCall(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "stored-model-per-call")
	require.NoError(t, h.app.Sessions.UpdateModels(t.Context(), sess.ID,
		&session.ModelSlotUpdate{Provider: "openaicompat", Model: "smart-a"}, nil))
	close(h.release)

	run := func(prompt string, overrides RunOverrides) {
		t.Helper()
		result, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            prompt,
			ContinueSessionID: sess.ID,
			Overrides:         overrides,
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		require.NoError(t, err)
		require.NotNil(t, result)
	}

	run("stored model must not override configured default", RunOverrides{})
	require.Equal(t, "smart-default", waitModelOverrideRequest(t, h.requests))
	run("explicit model", RunOverrides{SmartModel: "smart-b"})
	require.Equal(t, "smart-b", waitModelOverrideRequest(t, h.requests))
	run("no model flags after explicit override", RunOverrides{})
	require.Equal(t, "smart-default", waitModelOverrideRequest(t, h.requests))
}

func TestExecuteRunStoredEffortAndExplicitSmartEffortArePerCall(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "stored-effort-per-call")
	require.NoError(t, h.app.Sessions.UpdateReasoningEffort(t.Context(), sess.ID, "low", ""))

	resultCh := make(chan error, 1)
	run := func(prompt string, overrides RunOverrides) {
		t.Helper()
		go func() {
			_, err := h.app.ExecuteRun(context.Background(), RunRequest{
				Prompt: prompt, ContinueSessionID: sess.ID, Overrides: overrides,
				Mode: RunModeJSON, Stdout: io.Discard, Stderr: io.Discard,
				HideSpinner: true, FailIfSessionBusy: true,
			})
			resultCh <- err
		}()
	}
	requestEffort := func(model, effort string) {
		t.Helper()
		select {
		case got := <-h.requests:
			require.Equal(t, model, got)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for provider request")
		}
		select {
		case body := <-h.bodies:
			var got modelOverrideRequest
			require.NoError(t, json.Unmarshal(body, &got))
			require.Equal(t, effort, got.Effort)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for provider request body")
		}
	}

	// The provider answers at once: the runs are sequential on one App.
	close(h.release)
	run("configured effort despite stored effort", RunOverrides{})
	requestEffort("smart-default", "medium")
	require.NoError(t, waitModelOverrideResult(t, resultCh))

	run("explicit smart effort with fast-only override", RunOverrides{FastModel: "fast-a", ReasoningEffort: "high", RoleSmart: true})
	requestEffort("smart-default", "high")
	require.NoError(t, waitModelOverrideResult(t, resultCh))

	run("subsequent invocation uses configured effort", RunOverrides{})
	requestEffort("smart-default", "medium")
	require.NoError(t, waitModelOverrideResult(t, resultCh))
}

func TestExecuteRunModelOverridesArePinnedPerCallAndNotPersisted(t *testing.T) {
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
		gotModels[waitModelOverrideRequest(t, h.requests)] = true
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
	require.Empty(t, storedA.SmartModelID)
	require.Empty(t, storedA.FastModelID)
	require.Empty(t, storedB.SmartModelID)
	require.Empty(t, storedB.FastModelID)
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

	resultCh := make(chan error, 1)
	go func() {
		_, runErr := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "transaction failure",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{SmartModel: "smart-fail", FastModel: "fast-a"},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		resultCh <- runErr
	}()
	require.Equal(t, "smart-fail", waitModelOverrideRequest(t, h.requests))
	close(h.release)
	require.NoError(t, waitModelOverrideResult(t, resultCh))
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
	require.Equal(t, "smart-a", waitModelOverrideRequest(t, h.requests))
	close(h.release)
	require.NoError(t, waitModelOverrideResult(t, resultCh))

	after, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelID)
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
	require.Equal(t, "smart-a", waitModelOverrideRequest(t, h.requests))
	close(h.release)
	require.NoError(t, waitModelOverrideResult(t, resultCh))
	oldDB := h.app.DB()
	h.app.Shutdown()
	freshStore, err := config.Init(h.dataDir, h.dataDir, false)
	require.NoError(t, err)
	require.NotSame(t, h.store, freshStore)
	conn, err := db.Connect(t.Context(), h.dataDir)
	require.NoError(t, err)
	require.NotSame(t, oldDB, conn)
	fresh, err := New(t.Context(), conn, freshStore)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(fresh.Shutdown)
	after, err := fresh.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, after.SmartModelID)
	require.Empty(t, after.FastModelID)
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
	require.Equal(t, "smart-default", waitModelOverrideRequest(t, h.requests))
}

func TestExecuteRunQueuedOverrideDoesNotPersistWhenPromotedAndPromotionFails(t *testing.T) {
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
	require.Equal(t, "smart-a", waitModelOverrideRequest(t, h.requests))

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
	outcome := <-outcomes
	require.Equal(t, 2, outcome.idx)
	require.ErrorIs(t, outcome.err, ErrRunQueued)
	queued = true
	require.True(t, queued)
	close(h.release)
	require.NoError(t, (<-outcomes).err)
	require.Equal(t, "smart-b", waitModelOverrideRequest(t, h.requests))

	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SmartModelID)
	require.Empty(t, stored.FastModelID)
}

func TestExecuteRunQueuedOverrideDoesNotPersistWhenPromoted(t *testing.T) {
	h := newModelOverrideApp(t)
	sess := createModelOverrideSession(t, h.app, "queued-override")

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
	require.Equal(t, "smart-a", waitModelOverrideRequest(t, h.requests))
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
	require.ErrorIs(t, <-outcomes, ErrRunQueued)
	close(h.release)
	require.NoError(t, <-outcomes)
	require.Equal(t, "smart-b", waitModelOverrideRequest(t, h.requests))

	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SmartModelID)
	require.Empty(t, stored.FastModelID)
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
	require.Equal(t, "smart-a", waitModelOverrideRequest(t, h.requests))

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
	require.Empty(t, stored.SmartModelID)
	require.Empty(t, stored.FastModelID)

	close(h.release)
	require.NoError(t, <-firstDone)
}
