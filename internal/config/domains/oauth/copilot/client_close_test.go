package copilot

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// closeTrackingRoundTripper records CloseIdleConnections calls.
type closeTrackingRoundTripper struct {
	closed atomic.Int64
}

func (c *closeTrackingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (c *closeTrackingRoundTripper) CloseIdleConnections() { c.closed.Add(1) }

// TestInitiatorClientCloseIdleConnectionsReachesBase proves the F5
// wrapper fix for the Copilot initiator transport: the wrapper client's
// CloseIdleConnections must reach the base transport's pool.
func TestInitiatorClientCloseIdleConnectionsReachesBase(t *testing.T) {
	t.Parallel()

	base := &closeTrackingRoundTripper{}
	client := NewClientWithTransport(false, false, base)
	client.CloseIdleConnections()
	require.Equal(t, int64(1), base.closed.Load(),
		"initiator client's CloseIdleConnections must reach the base transport")
}
