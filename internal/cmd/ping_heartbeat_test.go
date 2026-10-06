// REVERT-CHECK: dropping the NewHeartbeatProvider wrap in buildPingProvider
// must fail TestPingProviderRecordsHeartbeat.
package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/stretchr/testify/require"
)

func TestPingProviderRecordsHeartbeat(t *testing.T) {
	t.Setenv("RUSH_HEARTBEAT_DIR", t.TempDir())
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.ResetForTest)
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Cleanup(func() { heartbeat.Shutdown(time.Second) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	store := config.NewTestStoreWithResolver(&config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{}),
	}, config.NewShellVariableResolver(env.NewFromMap(map[string]string{})))
	providerCfg := config.ProviderConfig{ID: "hbping", Type: openai.Name, APIKey: "k", BaseURL: server.URL}
	modelCfg := &config.SelectedModel{Provider: "hbping", Model: "ping-model"}
	ctx := heartbeat.WithContext(t.Context(), heartbeat.Context{
		SessionID: "ping-hb-test", Purpose: heartbeat.PurposePing, Role: "ping", Source: "per-call",
	})

	p, err := buildPingProvider(ctx, store, providerCfg, modelCfg, t.TempDir())
	require.NoError(t, err)
	lm, err := p.LanguageModel(ctx, "ping-model")
	require.NoError(t, err)
	_, err = lm.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")}})
	require.Error(t, err)

	heartbeat.Shutdown(2 * time.Second)
	entries, err := heartbeat.ReadAll()
	require.NoError(t, err)
	for _, e := range entries {
		if e.Session == "ping-hb-test" {
			require.EqualValues(t, 1, e.Totals.Requests)
			return
		}
	}
	t.Fatal("the ping's model call must land in the heartbeat snapshot")
}
