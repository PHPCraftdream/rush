package mcpdial

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestHeaderRoundTripperUsesOwnedTransportPool(t *testing.T) {
	first, err := cloneHTTPTransport()
	require.NoError(t, err)
	second, err := cloneHTTPTransport()
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.NotSame(t, http.DefaultTransport, first)
	require.NotSame(t, http.DefaultTransport, second)

	firstRT := &headerRoundTripper{transport: first}
	secondRT := &headerRoundTripper{transport: second}
	require.NotSame(t, firstRT.transport, secondRT.transport)
	firstRT.CloseIdleConnections()
	secondRT.CloseIdleConnections()
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCloneHTTPTransportFallsBackFromCustomDefault(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = testRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("custom default transport must not be used")
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	first, err := cloneHTTPTransport()
	require.NoError(t, err)
	second, err := cloneHTTPTransport()
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.True(t, first.ForceAttemptHTTP2)

	transport, err := CreateTransport(context.Background(), config.MCPConfig{
		Type: config.MCPHttp,
		URL:  "https://mcp.example.com",
	}, config.IdentityResolver())
	require.NoError(t, err)
	streamable, ok := transport.(*mcp.StreamableClientTransport)
	require.True(t, ok)
	roundTripper, ok := streamable.HTTPClient.Transport.(*headerRoundTripper)
	require.True(t, ok)
	require.NotSame(t, first, roundTripper.transport)
	require.NotNil(t, roundTripper.transport)
}
