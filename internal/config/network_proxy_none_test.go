package config

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

func TestIsProxyNone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"none", true},
		{"None", true},
		{"NONE", true},
		{"  none  ", true},
		{"", false},
		{"nonesuch", false},
		{"http://none:8080", false},
		{"socks5://127.0.0.1:1080", false},
		{"null", false},
		{"off", false},
	} {
		require.Equal(t, tc.want, IsProxyNone(tc.in), "%q", tc.in)
	}
}

// TestProxyNoneSurvivesKtavAndJSONDecoding makes sure the keyword reaches
// the config struct untouched from both on-disk formats (a ktav bare
// word must stay the string "none", not a null/boolean literal).
func TestProxyNoneSurvivesKtavAndJSONDecoding(t *testing.T) {
	t.Parallel()
	const ktavSrc = `
options: {
    network: {
        proxy: socks5://127.0.0.1:1080
    }
}
providers: {
    direct: {
        type: openai-compat
        base_url: https://api.example.com/v1
        network: {
            proxy: none
        }
    }
    tunneled: {
        type: openai-compat
        base_url: https://api.example.org/v1
        network: {
            proxy: http://proxy.corp.internal:8080
        }
    }
}
`
	tree, err := ktavcodec.DecodeKtav([]byte(ktavSrc))
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)

	var decoded struct {
		Options   Options                   `json:"options"`
		Providers map[string]ProviderConfig `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(jsonBytes, &decoded))
	require.NotNil(t, decoded.Options.Network)
	require.Equal(t, "socks5://127.0.0.1:1080", decoded.Options.Network.Proxy)
	require.NotNil(t, decoded.Providers["direct"].Network)
	require.True(t, IsProxyNone(decoded.Providers["direct"].Network.Proxy), "%q", decoded.Providers["direct"].Network.Proxy)
	require.Equal(t, "http://proxy.corp.internal:8080", decoded.Providers["tunneled"].Network.Proxy)

	// The JSON spelling decodes identically.
	var fromJSON struct {
		Providers map[string]ProviderConfig `json:"providers"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"providers":{"direct":{"network":{"proxy":"none"}}}}`), &fromJSON))
	require.True(t, IsProxyNone(fromJSON.Providers["direct"].Network.Proxy))
}

// TestProviderProxyNoneSurvivesConfigureProviders: the keyword is user
// data on both the known-provider and custom-provider rebuild paths; if
// configureProviders dropped or rewrote it the provider would silently
// fall back to the global proxy.
func TestProviderProxyNoneSurvivesConfigureProviders(t *testing.T) {
	known := []catwalk.Provider{{
		ID:          "openai",
		APIKey:      "$OPENAI_API_KEY",
		APIEndpoint: "https://api.openai.com/v1",
		Models:      []catwalk.Model{{ID: "test-model"}},
	}}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("openai", ProviderConfig{APIKey: "xyz", Network: &NetworkConfig{Proxy: "none"}})
	cfg.Providers.Set("custom", ProviderConfig{
		Type:    "openai-compat",
		BaseURL: "https://api.example.com/v1",
		APIKey:  "xyz",
		Models:  []catwalk.Model{{ID: "m"}},
		Network: &NetworkConfig{Proxy: "NONE"},
	})
	cfg.setDefaults("/tmp", "")
	cfg.Options.Network = &NetworkConfig{Proxy: "http://global-proxy.example:8080"}

	environ := env.NewFromMap(map[string]string{"OPENAI_API_KEY": "test-key"})
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), environ, NewShellVariableResolver(environ), known))

	for _, id := range []string{"openai", "custom"} {
		pc, ok := cfg.Providers.Get(id)
		require.True(t, ok, id)
		require.NotNil(t, pc.Network, id)
		require.True(t, IsProxyNone(pc.Network.Proxy), "%s: %q", id, pc.Network.Proxy)
	}
}
