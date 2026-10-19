// R12-H fix regression guard: a flag-ON App that ran InitCoderAgent must
// have the node-backed cutover facts bound, so its coordinator answers
// CLIScope from batch_nodes (zero state for a session with no debt and no
// nodes) instead of the conservative unknown fail-safe, whose permanent
// "node source unavailable" error made the CLI loop retry a wiring defect.
// Revert-check: removing the binding block in app_agent_setup.go turns this
// red (the unbound coordinator answers through unknownBatchCutoverFacts).
// No t.Parallel(): t.Setenv and shared package binding state.
package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestInitCoderAgent_FlagOn_BindsNodeBackedCutoverFacts(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	isolateAppNewTestEnv(t)

	dataDir := t.TempDir()

	// Minimal SSE chat-completion stub so InitCoderAgent's coordinator
	// construction has a provider to validate against, without hitting a
	// real network endpoint (same pattern as the App.New fixture tests).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
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
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Runs after application.Shutdown (LIFO).
	t.Cleanup(func() { _ = db.Release(dataDir) })

	application, err := New(context.Background(), conn, store, SkipAgentSetup())
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	require.NoError(t, application.InitCoderAgent(context.Background()))
	require.NotNil(t, application.AgentCoordinator)

	// No batch runtime installed, no debt, no batch nodes: the bound
	// node-backed facts answer the zero CLIScopeState with no error.
	// CLIScope lives on the ReactionDebtSource seam, not the Coordinator
	// interface (existing test fakes keep compiling).
	src, ok := application.AgentCoordinator.(agent.ReactionDebtSource)
	require.True(t, ok, "the built coordinator must satisfy ReactionDebtSource")
	state, err := src.CLIScope(context.Background(), "nonexistent-session")
	require.NoError(t, err)
	require.Equal(t, agent.CLIScopeState{}, state)
}
