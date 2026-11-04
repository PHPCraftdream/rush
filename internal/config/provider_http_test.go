package config

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type recordingTransport struct{ name string }

func (recordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}

func TestProviderHTTPClientPassesBothNetworkLevelsToTheFactory(t *testing.T) {
	var gotGlobal, gotProvider *NetworkConfig
	transport := recordingTransport{name: "factory"}
	t.Cleanup(SetProviderHTTPClientFactory(func(global, provider *NetworkConfig) (*http.Client, error) {
		gotGlobal, gotProvider = global, provider
		return &http.Client{Transport: transport}, nil
	}))

	globalNet := &NetworkConfig{Proxy: "socks5://global:1080"}
	providerNet := &NetworkConfig{Proxy: "none"}
	cfg := &Config{Options: &Options{Network: globalNet}}
	client, err := cfg.ProviderHTTPClient(ProviderConfig{Network: providerNet})
	require.NoError(t, err)
	require.NotNil(t, client)
	require.Same(t, globalNet, gotGlobal)
	require.Same(t, providerNet, gotProvider)
	require.Equal(t, transport, client.Transport)
	require.Equal(t, providerRequestTimeout, client.Timeout, "provider-bound requests are bounded")
}

func TestProviderHTTPClientToleratesMissingConfigAndOptions(t *testing.T) {
	var gotGlobal *NetworkConfig
	called := false
	t.Cleanup(SetProviderHTTPClientFactory(func(global, _ *NetworkConfig) (*http.Client, error) {
		called, gotGlobal = true, global
		return nil, nil
	}))

	for _, cfg := range []*Config{nil, {}, {Options: &Options{}}} {
		called, gotGlobal = false, &NetworkConfig{}
		client, err := cfg.ProviderHTTPClient(ProviderConfig{})
		require.NoError(t, err)
		require.Nil(t, client, "nothing configured means the default client")
		require.True(t, called)
		require.Nil(t, gotGlobal)
	}
}

func TestProviderHTTPClientReportsMalformedSettingsInsteadOfFallingBack(t *testing.T) {
	boom := errors.New("unsupported proxy scheme")
	t.Cleanup(SetProviderHTTPClientFactory(func(_, _ *NetworkConfig) (*http.Client, error) {
		return &http.Client{}, boom
	}))
	client, err := (&Config{}).ProviderHTTPClient(ProviderConfig{})
	require.ErrorIs(t, err, boom)
	require.Nil(t, client, "a malformed setting must never yield a usable client")
}

func TestProviderHTTPClientWithoutFactoryUsesDefaultClient(t *testing.T) {
	restore := SetProviderHTTPClientFactory(nil)
	t.Cleanup(restore)
	client, err := (&Config{}).ProviderHTTPClient(ProviderConfig{Network: &NetworkConfig{Proxy: "http://p:1"}})
	require.NoError(t, err)
	require.Nil(t, client)
}
