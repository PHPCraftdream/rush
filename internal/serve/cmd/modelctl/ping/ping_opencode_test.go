package ping

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
	"github.com/stretchr/testify/require"
)

// REVERT-CHECK: dropping WithOpenCodeSessionHeader in buildPingProviderRaw
// must fail this test (the gateway rejects a request without the header).
func TestPingProviderSendsOpenCodeSessionHeader(t *testing.T) {
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

	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{}),
	}, config.NewShellVariableResolver(env.NewFromMap(map[string]string{})))
	providerCfg := config.ProviderConfig{ID: "opencode-go", Type: openaicompat.Name, APIKey: "k", BaseURL: server.URL}
	modelCfg := &config.SelectedModel{Provider: "opencode-go", Model: "deepseek-v4.1-flash"}

	p, err := buildPingProviderRaw(t.Context(), store, providerCfg, modelCfg, t.TempDir())
	require.NoError(t, err)
	lm, err := p.LanguageModel(t.Context(), "deepseek-v4.1-flash")
	require.NoError(t, err)
	_, err = lm.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}})
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, sessions, "the ping must reach the server")
	for _, s := range sessions {
		require.Equal(t, modelkit.OpenCodeProcessSessionID(), s)
	}
}
