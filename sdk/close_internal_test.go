package sdk

// Internal-construction tests for Client.Close's idempotency and
// nil-safety contracts. They live in package sdk (not sdk_test) because
// building a Client around a stub Coordinator requires setting the
// unexported app field directly — sdk.Wrap, the previous external
// shortcut, was removed from the public API (review R1-7).

import (
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/require"
)

// closeSpyCoordinator counts CancelAll calls — the first thing an App
// shutdown performs — and reports graceful (not busy) every time. The
// embedded nil Coordinator is never reached: shutdown touches no other
// Coordinator method.
type closeSpyCoordinator struct {
	agent.Coordinator
	cancelAllCalls atomic.Int32
}

func (c *closeSpyCoordinator) CancelAll() bool {
	c.cancelAllCalls.Add(1)
	return false
}

// TestClientCloseIsIdempotent pins the idempotency contract of
// Client.Close: shutdown runs exactly once and every subsequent call
// returns the same cached CloseResult.
func TestClientCloseIsIdempotent(t *testing.T) {
	spy := &closeSpyCoordinator{}
	client := &Client{app: &app.App{AgentCoordinator: spy}}

	first := client.Close()
	second := client.Close()

	require.Equal(t, int32(1), spy.cancelAllCalls.Load(), "second Close must not re-run shutdown")
	require.False(t, first.Forced)
	require.Empty(t, first.CleanupErrors)
	require.Equal(t, first, second, "repeat Close must return the cached result")
}

// TestClientCloseNilReceiverAndNilApp pins the nil-safety contract of
// Client.Close: closing a nil client or a client wrapping a nil App is a
// no-op that returns an empty, non-forced result.
func TestClientCloseNilReceiverAndNilApp(t *testing.T) {
	var nilClient *Client
	res := nilClient.Close()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)

	res = (&Client{}).Close()
	require.False(t, res.Forced)
	require.Empty(t, res.CleanupErrors)
}
