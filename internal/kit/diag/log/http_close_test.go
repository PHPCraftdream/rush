package log

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// closeTrackingRoundTripper records CloseIdleConnections calls; it
// stands in for the real transport whose pool must actually be
// reached through the wrapper chain.
type closeTrackingRoundTripper struct {
	closed atomic.Int64
}

func (c *closeTrackingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrHandlerTimeout
}

func (c *closeTrackingRoundTripper) CloseIdleConnections() { c.closed.Add(1) }

// TestWrapperClientCloseIdleConnectionsReachesBaseTransport proves the
// F5 wrapper fix: http.Client.CloseIdleConnections only consults its
// Transport field's own CloseIdleConnections method, so without the
// forwarding methods the call silently does nothing.
func TestWrapperClientCloseIdleConnectionsReachesBaseTransport(t *testing.T) {
	t.Parallel()

	base := &closeTrackingRoundTripper{}
	client := NewHTTPClientWithTransport(base)
	client.CloseIdleConnections()
	require.Equal(t, int64(1), base.closed.Load(),
		"wrapper client's CloseIdleConnections must reach the wrapped transport")
}
