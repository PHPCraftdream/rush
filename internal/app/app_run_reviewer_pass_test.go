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
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/assert"
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
	toolSets [][]string
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

func (h *reviewerPassApp) requestedToolSets() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([][]string, len(h.toolSets))
	copy(out, h.toolSets)
	return out
}

// reviewerPassAppOpts selects which model slots the harness configures.
type reviewerPassAppOpts struct {
	withReviewer bool
	withWorker   bool
}

func newReviewerPassApp(t *testing.T, withReviewer bool) *reviewerPassApp {
	t.Helper()
	return newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: withReviewer})
}

func newReviewerPassAppOpts(t *testing.T, opts reviewerPassAppOpts) *reviewerPassApp {
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
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		toolNames := make([]string, 0, len(req.Tools))
		for _, tl := range req.Tools {
			if tl.Function.Name != "" {
				toolNames = append(toolNames, tl.Function.Name)
			}
		}
		h.mu.Lock()
		h.requests = append(h.requests, req.Model)
		h.bodies = append(h.bodies, string(body))
		h.toolSets = append(h.toolSets, toolNames)
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

	selected := []string{
		`"smart": {"provider":"openaicompat","model":"smart-default"}`,
		`"fast": {"provider":"openaicompat","model":"fast-default"}`,
	}
	providerModels := []string{
		`{"id":"smart-default","context_window":200000,"default_max_tokens":1000}`,
		`{"id":"fast-default","context_window":200000,"default_max_tokens":1000}`,
	}
	if opts.withReviewer {
		selected = append(selected, `"reviewer": {"provider":"openaicompat","model":"`+reviewerPassReviewerModel+`"}`)
		providerModels = append(providerModels, `{"id":"`+reviewerPassReviewerModel+`","context_window":200000,"default_max_tokens":1000}`)
	}
	if opts.withWorker {
		selected = append(selected, `"worker": {"provider":"openaicompat","model":"reviewer-pass-worker"}`)
		providerModels = append(providerModels, `{"id":"reviewer-pass-worker","context_window":200000,"default_max_tokens":1000}`)
	}
	modelsJSON := `"models": {
    ` + strings.Join(selected, `,
    `) + `
  }`
	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        %s
      ]
    }
  },
  %s
}`, srv.URL, strings.Join(providerModels, `,
        `), modelsJSON)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	if opts.withReviewer {
		store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: reviewerPassReviewerModel})
	}
	if opts.withWorker {
		store.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{Provider: "openaicompat", Model: "reviewer-pass-worker"})
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

// TestExecuteRunReviewerPassTurnUsesReviewerCallOptions pins F1
// (2026-09-21 weekly audit): the review turn must EXECUTE under the
// review context (ModelRole=reviewer, DisableSubAgents=true), not under
// the primary phase's stale context. With a worker configured the
// primary --role smart turn runs in orchestrator mode (worker-delegation
// `agent` tool present, direct edit tools stripped); if the review turn
// reused that context it would present the same orchestrator toolset.
// The fix makes resetForReviewerPass assign reviewCtx to the loop's ctx,
// so the review request must instead carry a plain reviewer toolset.
func TestExecuteRunReviewerPassTurnUsesReviewerCallOptions(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: true, withWorker: true})
	sess := createModelOverrideSession(t, h.app, "reviewer-pass-toolset")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels(),
		"the clean smart run must be followed by exactly one review turn on the reviewer model")

	sets := h.requestedToolSets()
	require.Len(t, sets, 2, "both the primary and the review turn must present a toolset")

	// Primary: --role smart with a worker configured runs in orchestrator
	// mode — worker delegation present, direct edit tools stripped.
	assert.Contains(t, sets[0], "agent", "primary orchestrator turn must keep the delegation tool")
	assert.NotContains(t, sets[0], "edit")
	assert.NotContains(t, sets[0], "multiedit")
	assert.NotContains(t, sets[0], "write")

	// Review turn: must run as a plain reviewer call — edit tools back,
	// sub-agent tools gone. Before the F1 fix the review turn executed
	// under the primary's CallOptions and this assertion failed.
	assert.Contains(t, sets[1], "edit", "the review turn must not run in orchestrator mode")
	assert.NotContains(t, sets[1], "agent", "the review turn must not carry the worker-delegation tool")
	assert.NotContains(t, sets[1], "agentic_fetch")

	require.Equal(t, reviewerPassReviewerText, result.FinalText)
}

// TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim pins R2-3:
// the SDK's FailIfSessionBusy contract must survive the primary ->
// reviewer phase handoff. Caller A (fail-fast, smart + reviewer) finishes
// its primary phase; before A's review turn is admitted, caller B claims
// the same session via ReserveExclusive — the same atomic claim
// ExecuteRun itself uses. A's ExecuteRun must then fail fast with an
// error wrapping agent.ErrSessionBusy — NOT queue — and nothing of A's
// review turn may ever execute, not when B releases and not when B's own
// queue drains afterwards: the review turn carries write/bash tools, so
// a queued reviewer call executing under B's lifecycle after A already
// received a failure is exactly the violation this pins.
func TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-pass-fail-fast")

	origSeam := executeRunBeforeTurnLaunchSeam
	var launches atomic.Int32
	readyForB := make(chan struct{})
	bClaimed := make(chan struct{})
	executeRunBeforeTurnLaunchSeam = func() {
		if launches.Add(1) == 2 {
			// A's primary phase returned; the review turn is about to
			// launch. Hold it here until B owns the session.
			close(readyForB)
			<-bClaimed
		}
	}
	t.Cleanup(func() { executeRunBeforeTurnLaunchSeam = origSeam })

	type runOutcome struct {
		result *RunResult
		err    error
	}
	resultA := make(chan runOutcome, 1)
	go func() {
		res, err := h.app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "do the thing",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		resultA <- runOutcome{res, err}
	}()

	<-readyForB
	holdCtx, epoch, cancel, ok := h.app.AgentCoordinator.ReserveExclusive(context.Background(), sess.ID)
	require.True(t, ok, "B must be able to claim the session in the window between A's phases")
	close(bClaimed)
	require.NoError(t, holdCtx.Err(), "B's exclusive hold must be live while A is refused")

	outcome := <-resultA
	require.Error(t, outcome.err, "A's run must fail fast once B owns the session")
	require.ErrorIs(t, outcome.err, agent.ErrSessionBusy,
		"the SDK fail-fast contract must surface agent.ErrSessionBusy from the reviewer phase")
	require.NotErrorIs(t, outcome.err, ErrRunQueued,
		"A's review turn must be REFUSED, not queued behind B's ownership")

	require.NotContains(t, h.requestedModels(), reviewerPassReviewerModel,
		"A's reviewer call must not reach the provider while B owns the session")

	// Release B's claim and let B's own queue drain. Before the fix A's
	// reviewer call sat in the mailbox's submitted queue and the release
	// handed it to a fresh detached run — executed long after A was gone.
	h.app.AgentCoordinator.ReleaseExclusive(sess.ID, epoch, cancel)

	resultB, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{SmartModel: "openaicompat/fast-default"})
	require.NoError(t, err)
	require.NotNil(t, resultB)

	models := h.requestedModels()
	require.Equal(t, []string{"smart-default", "fast-default"}, models,
		"exactly A's primary turn and B's own turn may reach the provider — A's reviewer call must never execute")
	require.Equal(t, reviewerPassPrimaryText, resultB.FinalText,
		"B's own turn must own the envelope, not a deferred reviewer call from A")
}
