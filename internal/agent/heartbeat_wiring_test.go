// REVERT-CHECK: returning the unwrapped provider from buildProviderWithValues
// must fail TestHbCoordinatorProvidersAreRecorded.
package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestHbCoordinatorProvidersAreRecorded(t *testing.T) {
	hbIsolate(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	coord := &coordinator{cfg: credentialLiteralStore(&credentialLiteralProbe{})}
	providerCfg := config.ProviderConfig{ID: "hb-wire", Type: openai.Name, APIKey: "k", BaseURL: server.URL}

	provider, err := coord.buildProvider(coord.cfg.Config(), providerCfg, config.SelectedModel{}, false)
	require.NoError(t, err)
	ctx := hbAttributed("hb-wire")
	lm, err := provider.LanguageModel(ctx, "wire-model")
	require.NoError(t, err)
	_, err = lm.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
	require.Error(t, err)

	entry := hbFindEntry(t, hbShutdownFlush(t), "hb-wire")
	require.NotNil(t, entry, "a coordinator-built provider must record its model calls")
	require.EqualValues(t, 1, entry.Totals.Requests)
	require.EqualValues(t, 1, entry.Totals.Errors)
}
