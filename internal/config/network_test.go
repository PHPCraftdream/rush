package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkConfigUnmarshal(t *testing.T) {
	t.Parallel()

	const raw = `{"proxy":"socks5://127.0.0.1:1080","dns_server":"1.1.1.1","doh_url":"https://dns.example.test/dns-query"}`
	want := NetworkConfig{
		Proxy:     "socks5://127.0.0.1:1080",
		DNSServer: "1.1.1.1",
		DoHURL:    "https://dns.example.test/dns-query",
	}

	tests := []struct {
		name    string
		json    string
		target  func() any
		extract func(t *testing.T, target any) NetworkConfig
	}{
		{
			name:   "standalone",
			json:   raw,
			target: func() any { return &NetworkConfig{} },
			extract: func(_ *testing.T, target any) NetworkConfig {
				return *target.(*NetworkConfig)
			},
		},
		{
			name:   "inside options",
			json:   `{"network":` + raw + `}`,
			target: func() any { return &Options{} },
			extract: func(t *testing.T, target any) NetworkConfig {
				network := target.(*Options).Network
				require.NotNil(t, network)
				return *network
			},
		},
		{
			name:   "inside provider",
			json:   `{"network":` + raw + `}`,
			target: func() any { return &ProviderConfig{} },
			extract: func(t *testing.T, target any) NetworkConfig {
				network := target.(*ProviderConfig).Network
				require.NotNil(t, network)
				return *network
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target := tt.target()
			require.NoError(t, json.Unmarshal([]byte(tt.json), target))
			require.Equal(t, want, tt.extract(t, target))
		})
	}
}

func TestNetworkConfigOmitEmpty(t *testing.T) {
	t.Parallel()

	marshal := func(t *testing.T, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return string(b)
	}

	tests := []struct {
		name string
		json string
	}{
		{"options zero value", marshal(t, Options{})},
		{"provider zero value", marshal(t, ProviderConfig{})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.NotContains(t, tt.json, `"network"`)
		})
	}

	t.Run("non-nil empty struct appears", func(t *testing.T) {
		t.Parallel()
		require.Contains(t, marshal(t, Options{Network: &NetworkConfig{}}), `"network"`)
		require.Contains(t, marshal(t, ProviderConfig{Network: &NetworkConfig{}}), `"network"`)
	})
}
