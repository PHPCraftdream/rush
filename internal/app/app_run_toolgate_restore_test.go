package app

// Regression test for risk R3 of
// docs/plans/2026-08-29-embeddable-library-refactoring.md: ExecuteRun's
// DisableSubAgents gate strips agent/agentic_fetch from the coder's
// PUBLISHED AllowedTools (disableSubAgentToolsInConfig →
// UpdateAgentAllowedTools) and, before the fix, never restored them. For
// the one-shot `rush run` process that was moot; for a library caller
// (sdk.Client) making two runs on one *App, the second run inherited the
// first run's stripped toolset. The fix snapshots the coder's
// AllowedTools before the gate mutates it and restores it via defer when
// ExecuteRun returns. This test pins the observable behavior: after a
// DisableSubAgents run RETURNS, the config must already be back to the
// original list, and a follow-up run without the flag must see (and
// keep) the full toolset.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunRestoresAllowedToolsAfterDisableSubAgents(t *testing.T) {
	// Same global-config/data isolation as the golden envelope test —
	// both GlobalConfig() and GlobalConfigData() resolution paths must
	// be isolated before app.New runs.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	// Single-phase mock provider: every request gets a plain final-text
	// answer. The runs here exist only to drive ExecuteRun through its
	// gate/restore lifecycle; no tool calls are needed, so the mock is
	// deliberately simpler than the golden test's two-phase one.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"gate ok"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	// A non-default title plus one pre-existing message keep needsTitle
	// false, so the background title-generation provider call never
	// fires (same trick as the golden envelope test).
	sess, err := application.Sessions.Create(context.Background(), "gate-restore-title")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	allowedTools := func() []string {
		return application.config.Config().Agents[config.AgentCoder].AllowedTools
	}

	before := slices.Clone(allowedTools())
	require.Contains(t, before, "agent", "precondition: coder starts with the agent tool")
	require.Contains(t, before, "agentic_fetch", "precondition: coder starts with agentic_fetch")

	// Run 1: DisableSubAgents — the gate strips both tools for the
	// duration of the run, and the fix restores them by the time the
	// call returns. Before the fix this failed: the strip outlived the
	// run.
	res1, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "say gate ok",
		Mode:              RunModeJSON,
		ContinueSessionID: sess.ID,
		HideSpinner:       true,
		Overrides:         RunOverrides{DisableSubAgents: true},
	})
	require.NoError(t, err)
	require.NotNil(t, res1)
	require.Equal(t, "end_turn", res1.ExitReason)

	afterRun1 := allowedTools()
	require.Contains(t, afterRun1, "agent",
		"agent tool must be restored once the DisableSubAgents run returns (R3: it used to stay stripped)")
	require.Contains(t, afterRun1, "agentic_fetch",
		"agentic_fetch must be restored once the DisableSubAgents run returns (R3: it used to stay stripped)")
	require.Equal(t, before, afterRun1, "restored AllowedTools must equal the original list")

	// Run 2: no DisableSubAgents — must see (and keep) the full toolset;
	// this is exactly the call that pre-fix code silently broke.
	res2, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "say gate ok again",
		Mode:              RunModeJSON,
		ContinueSessionID: sess.ID,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.Equal(t, "end_turn", res2.ExitReason)

	afterRun2 := allowedTools()
	require.Equal(t, before, afterRun2,
		"a run without DisableSubAgents must neither inherit nor extend the gate mutation")
}
