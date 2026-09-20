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
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// The end-to-end reviewer-pass tests. They reuse the established pattern
// of app_run_model_overrides_test.go: a REAL App whose provider is a stub
// openai-compat httptest server, keyed by the requested model id, so the
// tests observe exactly which model each turn of ExecuteRun ran and what
// the final envelope carries. The coordinator is NOT faked: message
// persistence, terminal reconciliation and the two-phase event loop all
// run for real.

const (
	reviewerPassPrimaryText   = "PRIMARY ANSWER: the work is done."
	reviewerPassReviewerText  = "REVIEWER VERDICT: work verified, no gaps found."
	reviewerPassReviewerModel = "reviewer-r"
)

type reviewerPassApp struct {
	app   *App
	store *config.ConfigStore

	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (h *reviewerPassApp) requestedModels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

func (h *reviewerPassApp) recordedBodies() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.bodies, "\n---\n")
}

func newReviewerPassApp(t *testing.T, withReviewer bool) *reviewerPassApp {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	h := &reviewerPassApp{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		h.mu.Lock()
		h.requests = append(h.requests, req.Model)
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()
		content := reviewerPassPrimaryText
		if req.Model == reviewerPassReviewerModel {
			content = reviewerPassReviewerText
		}
		contentJSON, mErr := json.Marshal(content)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(srv.Close)

	modelsJSON := `"models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"}
  }`
	if withReviewer {
		modelsJSON = `"models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"},
    "reviewer": {"provider":"openaicompat","model":"` + reviewerPassReviewerModel + `"}
  }`
	}
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
        {"id":%q,"context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  %s
}`, srv.URL, reviewerPassReviewerModel, modelsJSON)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	if withReviewer {
		store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: reviewerPassReviewerModel})
	}
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	h.app = application
	h.store = store
	return h
}

func runReviewerPassExecuteRun(t *testing.T, h *reviewerPassApp, sessID string, overrides RunOverrides) (*RunResult, error) {
	t.Helper()
	return h.app.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "do the thing",
		ContinueSessionID: sessID,
		Overrides:         overrides,
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
}

// TestExecuteRunReviewerPassContinuesCleanSmartRunWithReviewerModel is the
// end-to-end proof: a clean --role smart run with a Reviewer model
// configured is followed by ONE more turn on the reviewer model, carrying
// the fixed reviewer prompt, and that reviewer turn's own response — not
// the primary turn's — becomes the envelope's final_text. The reviewer
// override must be a one-off: nothing may be persisted onto the session's
// model slots, so a later plain `rush run --session <same-id>` still
// resolves the session's normal smart model.
func TestExecuteRunReviewerPassContinuesCleanSmartRunWithReviewerModel(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-pass")

	before, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels(),
		"the clean smart run must be followed by exactly one more turn, on the reviewer model")
	require.Contains(t, h.recordedBodies(), "independent reviewer",
		"the second turn must carry the fixed reviewer-pass prompt")

	require.Equal(t, reviewerPassReviewerText, result.FinalText,
		"the reviewer's own response must become the run's final output, not the primary turn's")
	require.Equal(t, "end_turn", result.ExitReason, "a clean openai-compat run ends with the end_turn finish reason")

	stored, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SmartModelID, "the reviewer override must never be persisted on the session")
	require.Empty(t, stored.FastModelID)
	require.Equal(t, before.SmartModelReasoningEffort, stored.SmartModelReasoningEffort,
		"the reviewer pass must not change the session's stored reasoning effort (fresh rows carry the DB default %q)", before.SmartModelReasoningEffort)
	require.Equal(t, before.FastModelReasoningEffort, stored.FastModelReasoningEffort)
}

// TestExecuteRunNoReviewerConfiguredIsByteIdentical is the regression
// guard: with no Reviewer model configured, a --role smart run executes
// exactly one turn and the envelope carries the primary text.
func TestExecuteRunNoReviewerConfiguredIsByteIdentical(t *testing.T) {
	h := newReviewerPassApp(t, false)
	sess := createModelOverrideSession(t, h.app, "no-reviewer")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, []string{"smart-default"}, h.requestedModels(),
		"without a configured reviewer there must be exactly one provider turn")
	require.Equal(t, reviewerPassPrimaryText, result.FinalText)
	require.Equal(t, "end_turn", result.ExitReason, "a clean openai-compat run ends with the end_turn finish reason")
}

// TestExecuteRunExplicitReviewerRoleIsNotAutoFollowed pins the other half
// of the trigger: an explicit --role reviewer invocation (which the CLI
// folds into the SmartModel override) is NEVER auto-followed by a second
// review turn — the operator already chose the reviewer slot on purpose.
func TestExecuteRunExplicitReviewerRoleIsNotAutoFollowed(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "explicit-reviewer")

	overrides := RunOverrides{
		ModelRole:  config.SelectedModelTypeReviewer,
		SmartModel: "openaicompat/" + reviewerPassReviewerModel,
	}
	result, err := runReviewerPassExecuteRun(t, h, sess.ID, overrides)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, []string{reviewerPassReviewerModel}, h.requestedModels(),
		"an explicit --role reviewer run must not be followed by an automatic review pass")
	require.Equal(t, reviewerPassReviewerText, result.FinalText)
	require.Equal(t, "end_turn", result.ExitReason, "a clean openai-compat run ends with the end_turn finish reason")
}
