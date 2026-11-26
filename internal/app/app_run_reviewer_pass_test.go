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
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
	reviewerPassReviewerText  = "VERDICT: PASS"
	reviewerPassReviewerModel = "reviewer-r"
)

type reviewerPassApp struct {
	app   *App
	store *config.ConfigStore

	mu                     sync.Mutex
	requests               []string
	bodies                 []string
	toolSets               [][]string
	afterReviewerResponse  func()
	beforeReviewerResponse func()
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

func (h *reviewerPassApp) reviewerBodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var bodies []string
	for i, model := range h.requests {
		if model == reviewerPassReviewerModel && i < len(h.bodies) {
			bodies = append(bodies, h.bodies[i])
		}
	}
	return bodies
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
	withReviewer       bool
	withWorker         bool
	noReviewerToolCall bool
	// reviewerEffort, when set, is the reviewer slot's reasoning effort.
	reviewerEffort string
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
			Model    string `json:"model"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Stream    bool `json:"stream"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
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
		reviewerRequest := 0
		reviewerPrompt := false
		toolResultSeen := false
		for _, model := range h.requests {
			if model == reviewerPassReviewerModel {
				reviewerRequest++
			}
		}
		for _, msg := range req.Messages {
			if strings.Contains(string(msg.Content), "independent reviewer") {
				reviewerPrompt = true
			}
			if msg.Role == "tool" || strings.Contains(string(msg.Content), "read-file") {
				toolResultSeen = true
			}
		}
		h.mu.Unlock()
		if req.Model == reviewerPassReviewerModel && reviewerPrompt && toolResultSeen && h.beforeReviewerResponse != nil {
			h.beforeReviewerResponse()
		}
		content := reviewerPassPrimaryText
		if req.Model == reviewerPassReviewerModel {
			content = reviewerPassReviewerText
		}
		contentJSON, mErr := json.Marshal(content)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		if req.Model == reviewerPassReviewerModel && reviewerPrompt && reviewerRequest == 1 && !opts.noReviewerToolCall {
			toolArgs, err := json.Marshal(map[string]string{"file_path": "README.md"})
			require.NoError(t, err)
			toolArgsJSON, err := json.Marshal(string(toolArgs))
			require.NoError(t, err)
			toolDelta := `,"tool_calls":[{"index":0,"id":"read-file","type":"function","function":{"name":"view","arguments":` + string(toolArgsJSON) + `}}]`
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"`+toolDelta+`},"finish_reason":null}]}`)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		} else if req.Model == reviewerPassReviewerModel && reviewerPrompt && toolResultSeen {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		} else {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rp","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}
		_, _ = fmt.Fprintln(w, "data: [DONE]")
		if req.Model == reviewerPassReviewerModel && reviewerPrompt && toolResultSeen && h.afterReviewerResponse != nil {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			h.afterReviewerResponse()
		}
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
		providerModels = append(providerModels, `{"id":"`+reviewerPassReviewerModel+`","context_window":200000,"default_max_tokens":1000,"cost_per_1m_in":1,"cost_per_1m_out":1}`)
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
		store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: reviewerPassReviewerModel, ReasoningEffort: opts.reviewerEffort})
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

// TestExecuteRunFreshReviewerCancellationKeepsExecutorAnswer covers cancellation during the review handoff.
// The completed-review cancellation leg (P1-b ii attach branch with runTimeoutCommitted) has no deterministic
// ExecuteRun seam and is covered by code inspection only.
func TestExecuteRunFreshReviewerCancellationKeepsExecutorAnswer(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-canceled")
	require.NoError(t, h.store.SetConfigFields(config.ScopeWorkspace, map[string]any{"options.reviewer_session_mode": "fresh"}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.beforeReviewerResponse = cancel
	result, err := h.app.ExecuteRun(ctx, RunRequest{
		Prompt: "do the thing", ContinueSessionID: sess.ID,
		Overrides: RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:      RunModeJSON, Stdout: io.Discard, Stderr: io.Discard,
		captureResult: true,
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, result)
	require.Equal(t, sess.ID, result.SessionID)
	require.Equal(t, reviewerPassPrimaryText, result.FinalText)
	require.Equal(t, "error", result.ReviewVerdict)
	require.Contains(t, strings.Join(result.Warnings, " | "), "reviewer pass failed")
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
// the fixed reviewer prompt. A10: the reviewer's verdict lands in the
// envelope's additive review field while final_text stays the primary
// turn's answer. The reviewer
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

	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels(),
		"automatic review first reads, then supplies its verdict")
	require.Contains(t, h.recordedBodies(), "independent reviewer",
		"the second turn must carry the fixed reviewer-pass prompt")

	// A10: final_text stays the EXECUTOR's answer; the reviewer's verdict
	// moved to the additive review field. Orchestrators read final_text as
	// the work report, not the review.
	require.Equal(t, reviewerPassPrimaryText, result.FinalText,
		"final_text must stay the executor's own answer (A10)")
	require.Equal(t, "pass", result.ReviewVerdict)
	require.Contains(t, result.Review, "VERDICT: PASS")
	require.Len(t, h.requestedModels(), 3)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())
	require.NotContains(t, result.Review, reviewerPassPrimaryText)
	require.Equal(t, "end_turn", result.ExitReason, "a clean openai-compat run ends with the end_turn finish reason")
	reviewerBodies := h.reviewerBodies()
	require.NotEmpty(t, reviewerBodies)
	for _, body := range reviewerBodies {
		require.NotContains(t, body, "seed")
		require.Contains(t, body, "reviewer package")
	}
	require.Equal(t, sess.ID, result.SessionID)
	require.NotEmpty(t, result.ReviewerSessionID)
	reviewerChild, err := h.app.Sessions.Get(context.Background(), result.ReviewerSessionID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, reviewerChild.ParentSessionID)
	reviewerMsgs, err := h.app.Messages.List(context.Background(), reviewerChild.ID)
	require.NoError(t, err)
	for _, msg := range reviewerMsgs {
		require.NotContains(t, msg.FullText(), "seed")
	}

	stored, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored.SmartModelID, "the reviewer override must never be persisted on the session")
	require.Empty(t, stored.FastModelID)
	require.Equal(t, before.SmartModelReasoningEffort, stored.SmartModelReasoningEffort,
		"the reviewer pass must not change the session's stored reasoning effort (fresh rows carry the DB default %q)", before.SmartModelReasoningEffort)
	require.Equal(t, before.FastModelReasoningEffort, stored.FastModelReasoningEffort)
}

func TestExecuteRunReviewerPassFreshChildContract(t *testing.T) {
	h := newReviewerPassApp(t, true)
	if reviewerSessionMode(h.store.Config()) != "fresh" {
		t.Fatal("empty mode should use fresh behavior")
	}
	sess := createModelOverrideSession(t, h.app, "reviewer-fresh-contract")
	parentBefore, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	parentOwnBefore := parentBefore.OwnCost
	parentBase := parentBefore.CostBase
	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, sess.ID, result.SessionID)
	require.NotEmpty(t, result.ReviewerSessionID)
	child, err := h.app.Sessions.Get(context.Background(), result.ReviewerSessionID)
	require.NoError(t, err)
	// The cost-parent edge is proven below by the parent's subtree-spend delta.
	require.Greater(t, child.OwnCost, 0.0)
	reviewerMessages, err := h.app.Messages.List(context.Background(), child.ID)
	require.NoError(t, err)
	require.Len(t, reviewerMessages, 4)
	for _, msg := range reviewerMessages {
		require.NotContains(t, msg.FullText(), "seed")
	}
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())
	subtreeSpent, err := h.app.Sessions.SubtreeSpent(context.Background(), sess.ID)
	require.NoError(t, err)
	subtreeCost, err := h.app.Sessions.SubtreeBudget(context.Background(), sess.ID)
	require.NoError(t, err)
	childCost, err := h.app.Sessions.SubtreeSpent(context.Background(), child.ID)
	require.NoError(t, err)
	require.Equal(t, child.OwnCost, childCost)
	require.InDelta(t, child.OwnCost, subtreeCost, 1e-9)
	require.InDelta(t, child.OwnCost, subtreeSpent-parentBase, 1e-9)
	parentAfter, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, parentOwnBefore, parentAfter.OwnCost, 1e-9)
	require.Equal(t, sess.ID, result.SessionID)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())
}

func TestExecuteRunReviewerPassLegacyModeUsesParentSession(t *testing.T) {
	h := newReviewerPassApp(t, true)
	require.NoError(t, h.store.SetConfigFields(config.ScopeWorkspace, map[string]any{"options.reviewer_session_mode": "legacy"}))
	sess := createModelOverrideSession(t, h.app, "reviewer-legacy")
	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.ReviewerSessionID)
	require.Equal(t, sess.ID, result.SessionID)
	parent, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, parent.ID)
	require.NotEqual(t, "", parent.ID)
	msgs, err := h.app.Messages.List(context.Background(), sess.ID)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	parentTranscriptHasReview := false
	for _, msg := range msgs {
		if strings.Contains(msg.FullText(), "You are now acting as the independent reviewer") {
			parentTranscriptHasReview = true
		}
	}
	require.True(t, parentTranscriptHasReview)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels(),
		"legacy review reads then returns its verdict in the parent session")
}

func TestExecuteRunReviewerTurnUsesChildForEarlyPath(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-early")
	require.NoError(t, h.store.SetConfigFields(config.ScopeWorkspace, map[string]any{"options.reviewer_session_mode": "fresh"}))
	basis := &reviewBasis{Prompt: strings.Repeat("acceptance criteria ", 8000), WorkingDir: h.store.WorkingDir()}
	_, err := h.app.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "PRIMARY REPORT"}},
	})
	require.NoError(t, err)
	res, err := h.app.ExecuteRun(context.Background(), RunRequest{ContinueSessionID: sess.ID, Overrides: RunOverrides{}, Mode: RunModeJSON, Stdout: io.Discard, Stderr: io.Discard, captureResult: true, reviewerTurn: true, reviewerReport: "PRIMARY REPORT", reviewBasis: basis})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, sess.ID, res.SessionID)
	require.NotEmpty(t, res.ReviewerSessionID)
	require.Equal(t, []string{reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())
	toolSets := h.requestedToolSets()
	require.Len(t, toolSets, 2)
	require.Contains(t, toolSets[0], "view")
	warningText := strings.Join(res.Warnings, " ")
	assert.Contains(t, warningText, "original prompt middle")
	assert.Contains(t, warningText, "reviewer package truncated")
	assert.Contains(t, h.recordedBodies(), "reviewer package truncated")
	reviewerBodies := h.reviewerBodies()
	require.NotEmpty(t, reviewerBodies)
	for _, body := range reviewerBodies {
		assert.NotContains(t, body, "seed")
		assert.Contains(t, body, "reviewer package")
	}
	child, err := h.app.Sessions.Get(context.Background(), res.ReviewerSessionID)
	require.NoError(t, err)
	require.Equal(t, sess.ID, child.ParentSessionID)
	childMessages, _, err := h.app.Messages.ListWithWatermark(context.Background(), child.ID)
	require.NoError(t, err)
	var readCallCount, readResultCount int
	for _, msg := range childMessages {
		for _, part := range msg.Parts {
			if call, ok := part.(message.ToolCall); ok && call.Name == "view" {
				readCallCount++
			}
		}
		for _, result := range msg.ToolResults() {
			if result.Name == "view" {
				readResultCount++
			}
		}
	}
	assert.Equal(t, 1, readCallCount)
	assert.Equal(t, 1, readResultCount)
	require.Equal(t, "pass", res.ReviewVerdict)
	require.Contains(t, res.Review, "VERDICT: PASS")
	require.Equal(t, []string{reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())
}

func TestExecuteRunFreshReviewerRefusedWhenOwnerHoldsSession(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failIfBusy      bool
		wantQueued      bool
		wantSessionBusy bool
	}{
		{name: "default admission queues", wantQueued: true},
		{name: "fail-fast admission refuses", failIfBusy: true, wantSessionBusy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReviewerPassApp(t, true)
			sess := createModelOverrideSession(t, h.app, "reviewer-fresh-refused")
			require.NoError(t, h.store.SetConfigFields(config.ScopeWorkspace, map[string]any{"options.reviewer_session_mode": "fresh"}))
			_, err := h.app.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "PRIMARY ANSWER: untouched"}},
			})
			require.NoError(t, err)
			basis := &reviewBasis{Prompt: "do the thing", WorkingDir: h.store.WorkingDir()}

			holdCtx, epoch, release, admitted := h.app.AgentCoordinator.ReserveExclusive(context.Background(), sess.ID)
			require.True(t, admitted)
			defer h.app.AgentCoordinator.ReleaseExclusive(sess.ID, epoch, release)
			require.NoError(t, holdCtx.Err())
			_, err = h.app.ExecuteRun(context.Background(), RunRequest{
				ContinueSessionID: sess.ID, Overrides: RunOverrides{}, Mode: RunModeJSON,
				Stdout: io.Discard, Stderr: io.Discard, captureResult: true,
				FailIfSessionBusy: tc.failIfBusy,
				reviewerTurn:      true, reviewerReport: "PRIMARY ANSWER: untouched", reviewBasis: basis,
			})
			require.Error(t, err)
			if tc.wantQueued {
				require.ErrorIs(t, err, ErrRunQueued)
				require.NotErrorIs(t, err, agent.ErrSessionBusy)
			}
			if tc.wantSessionBusy {
				require.ErrorIs(t, err, agent.ErrSessionBusy)
				require.NotErrorIs(t, err, ErrRunQueued)
			}
			require.NotContains(t, h.requestedModels(), reviewerPassReviewerModel)
		})
	}
}

func TestExecuteRunReviewerPassSkippedWhenBudgetExhausted(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "reviewer-exhausted")
	require.NoError(t, h.app.Sessions.SetBudget(context.Background(), sess.ID, 1, 0, 0))
	_, err := h.app.Sessions.IncrementCost(context.Background(), sess.ID, 1)
	require.NoError(t, err)
	before, err := h.app.Sessions.ListSubSessions(context.Background(), sess.ID)
	require.NoError(t, err)
	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart, MaxCost: 1})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, sess.ID, result.SessionID)
	require.Empty(t, result.ReviewerSessionID)
	require.Contains(t, strings.Join(result.Warnings, " "), "budget exhausted")
	after, err := h.app.Sessions.ListSubSessions(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Len(t, after, len(before))
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
	require.Empty(t, result.Review, "no reviewer pass: the review field must stay empty")
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

	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels(),
		"the clean smart run is followed by two reviewer turns")

	sets := h.requestedToolSets()
	require.Len(t, sets, 3, "primary turn and both reviewer steps must present toolsets")

	// Primary: --role smart with a worker configured runs in orchestrator
	// mode — worker delegation present, direct edit tools stripped.
	assert.Contains(t, sets[0], "agent", "primary orchestrator turn must keep the delegation tool")
	assert.NotContains(t, sets[0], "edit")
	assert.NotContains(t, sets[0], "multiedit")
	assert.NotContains(t, sets[0], "write")

	// Review turn: must run as a plain reviewer call with a READ-ONLY
	// toolset — view/grep/glob/ls/git_read/read_delegation_transcript are
	// present, and every write, shell, delegation, network and question tool
	// is absent. That narrowing is exactly what
	// agent.applyCallReviewerReadOnly enforces for a reviewer-role top-level
	// call; the read-only set is reviewerReadOnlyToolNames.
	assert.NotContains(t, sets[1], "agent", "the review turn must not carry the worker-delegation tool")
	assert.NotContains(t, sets[1], "agentic_fetch")
	assert.NotContains(t, sets[1], "edit")
	assert.NotContains(t, sets[1], "multiedit")
	assert.NotContains(t, sets[1], "write")
	assert.NotContains(t, sets[1], "bash")
	assert.NotContains(t, sets[1], "run_command")
	assert.NotContains(t, sets[1], "todos")
	assert.NotContains(t, sets[1], "ask_question")
	assert.NotContains(t, sets[1], "download")
	assert.NotContains(t, sets[1], "fetch")
	assert.Contains(t, sets[1], "view", "the reviewer read step uses read-only tools")
	assert.NotContains(t, sets[2], "agent", "the verdict step remains reviewer-scoped")
	assert.NotContains(t, sets[2], "edit")
	assert.Contains(t, sets[2], "view", "the verdict step retains the read-only reviewer toolset")
	require.Equal(t, reviewerPassPrimaryText, result.FinalText,
		"A10: final_text stays the executor's answer")
	require.Equal(t, reviewerPassReviewerText, result.Review,
		"the review turn ran on the reviewer call options; its verdict is the review field")
}

// TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim pins R2-3:
// a fail-fast reviewer request must not queue behind an owner that holds
// the session lock before the review phase begins. In legacy mode, the review
// turn is admitted after the primary; in fresh mode the full pass reserves
// admission before creating/running its child. Both contracts forbid a
// reviewer provider call under a competing owner's claim.
func TestExecuteRunReviewerPassFailFastSurvivesInterPhaseClaim(t *testing.T) {
	h := newReviewerPassApp(t, true)
	require.NoError(t, h.store.SetConfigFields(config.ScopeWorkspace, map[string]any{"options.reviewer_session_mode": "legacy"}))
	if reviewerSessionMode(h.store.Config()) != "legacy" {
		t.Fatal("this inter-phase reservation test covers the legacy admission path")
	}
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
