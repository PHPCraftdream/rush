package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProviderDoesNotTreatCodexAPIKeyAsOAuth(t *testing.T) {
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	coord := &coordinator{cfg: store}
	_, err := coord.buildProvider(store.Config(), config.ProviderConfig{
		ID:     "openai-codex",
		Type:   openaicompat.Name,
		APIKey: "must-not-fall-back-to-an-API-key",
	}, config.SelectedModel{}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OAuth access token")
}

// REVERT-CHECK: dropping WrapOpenCodeHTTPClient in buildProviderWithValuesInner
// must fail this test: the OpenCode gateway answers 400 to a request without
// x-opencode-session.
func TestBuildProviderOpenCodeGoSendsConversationSession(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	var mu sync.Mutex
	var sessions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get(modelkit.OpenCodeSessionHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	coord := newWorkerToolTestCoordinator(t, testEnv(t), false)
	providerCfg := config.ProviderConfig{ID: "opencode-go", Type: openaicompat.Name, APIKey: "k", BaseURL: server.URL}
	model := config.SelectedModel{Provider: "opencode-go", Model: "deepseek-v4.1-flash"}
	provider, err := coord.buildProvider(coord.cfg.Config(), providerCfg, model, false)
	require.NoError(t, err)

	for _, sessionID := range []string{"conversation-1", "conversation-1", "conversation-2"} {
		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sessionID)
		lm, err := provider.LanguageModel(ctx, model.Model)
		require.NoError(t, err)
		_, err = lm.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
		require.Error(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"conversation-1", "conversation-1", "conversation-2"}, sessions)
}
