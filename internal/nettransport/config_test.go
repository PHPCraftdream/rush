package nettransport

import (
	"net/url"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// mustURL parses a test constant, panicking on failure; all inputs are
// compile-time literals known to be valid.
func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func TestResolveNetworkConfig(t *testing.T) {
	t.Parallel()

	newCfg := func(proxy, dns, doh string) *config.NetworkConfig {
		return &config.NetworkConfig{Proxy: proxy, DNSServer: dns, DoHURL: doh}
	}

	tests := []struct {
		name     string
		global   *config.NetworkConfig
		provider *config.NetworkConfig
		want     config.NetworkConfig
	}{
		{
			name:     "both nil yields zero config",
			global:   nil,
			provider: nil,
			want:     config.NetworkConfig{},
		},
		{
			name:     "global only inherited when provider nil",
			global:   newCfg("socks5://global:1080", "global.dns", "https://global.doh"),
			provider: nil,
			want: config.NetworkConfig{
				Proxy: "socks5://global:1080", DNSServer: "global.dns", DoHURL: "https://global.doh",
			},
		},
		{
			name:     "provider only when global nil",
			global:   nil,
			provider: newCfg("socks5://provider:1080", "provider.dns", "https://provider.doh"),
			want: config.NetworkConfig{
				Proxy: "socks5://provider:1080", DNSServer: "provider.dns", DoHURL: "https://provider.doh",
			},
		},
		{
			name:     "provider wins on every field when both set",
			global:   newCfg("socks5://global:1080", "global.dns", "https://global.doh"),
			provider: newCfg("socks5://provider:1080", "provider.dns", "https://provider.doh"),
			want: config.NetworkConfig{
				Proxy: "socks5://provider:1080", DNSServer: "provider.dns", DoHURL: "https://provider.doh",
			},
		},
		{
			name:     "merge is per field not per struct",
			global:   newCfg("", "global.dns", "https://global.doh"),
			provider: newCfg("socks5://provider:1080", "", ""),
			want: config.NetworkConfig{
				Proxy: "socks5://provider:1080", DNSServer: "global.dns", DoHURL: "https://global.doh",
			},
		},
		{
			name:     "reverse partial merge inherits provider doh",
			global:   newCfg("socks5://global:1080", "", ""),
			provider: newCfg("", "", "https://provider.doh"),
			want:     config.NetworkConfig{Proxy: "socks5://global:1080", DoHURL: "https://provider.doh"},
		},
		{
			name:     "empty provider inherits everything",
			global:   newCfg("socks5://global:1080", "global.dns", "https://global.doh"),
			provider: newCfg("", "", ""),
			want: config.NetworkConfig{
				Proxy: "socks5://global:1080", DNSServer: "global.dns", DoHURL: "https://global.doh",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ResolveNetworkConfig(tt.global, tt.provider)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestResolveConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cfg       config.NetworkConfig
		want      resolved
		wantEmpty bool
		errMsg    string
	}{
		{
			name:      "all empty is empty",
			cfg:       config.NetworkConfig{},
			want:      resolved{},
			wantEmpty: true,
		},
		{
			name: "doh url wins over dns server entirely",
			cfg: config.NetworkConfig{
				DNSServer: "1.1.1.1",
				DoHURL:    "https://cloudflare-dns.com/dns-query",
			},
			want: resolved{dohURL: "https://cloudflare-dns.com/dns-query"},
		},
		{
			name: "dns server without port gets default 53",
			cfg:  config.NetworkConfig{DNSServer: "1.1.1.1"},
			want: resolved{dnsServer: "1.1.1.1:53"},
		},
		{
			name: "dns hostname without port gets default 53",
			cfg:  config.NetworkConfig{DNSServer: "dns.example.test"},
			want: resolved{dnsServer: "dns.example.test:53"},
		},
		{
			name: "dns server with explicit port passes through",
			cfg:  config.NetworkConfig{DNSServer: "1.1.1.1:5353"},
			want: resolved{dnsServer: "1.1.1.1:5353"},
		},
		{
			name: "socks5 proxy parses with userinfo",
			cfg:  config.NetworkConfig{Proxy: "socks5://user:pass@127.0.0.1:1080"},
			want: resolved{proxyURL: mustURL("socks5://user:pass@127.0.0.1:1080")},
		},
		{
			name: "socks5h proxy scheme accepted",
			cfg:  config.NetworkConfig{Proxy: "socks5h://127.0.0.1:1080"},
			want: resolved{proxyURL: mustURL("socks5h://127.0.0.1:1080")},
		},
		{
			name:   "unsupported proxy scheme rejected",
			cfg:    config.NetworkConfig{Proxy: "ftp://127.0.0.1:21"},
			errMsg: "unsupported proxy scheme",
		},
		{
			name:   "malformed proxy url rejected",
			cfg:    config.NetworkConfig{Proxy: "http://[::1:1080"},
			errMsg: "parse proxy URL",
		},
		{
			name:   "proxy without host rejected",
			cfg:    config.NetworkConfig{Proxy: "socks5://"},
			errMsg: "has no host",
		},
		{
			name:   "malformed doh url rejected",
			cfg:    config.NetworkConfig{DoHURL: "https://[::1:443"},
			errMsg: "parse DoH URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveConfig(tt.cfg)
			if tt.errMsg != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tt.errMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.wantEmpty, got.empty())
		})
	}
}
