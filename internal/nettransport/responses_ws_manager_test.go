package nettransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func responsesWSManagerFixture(t *testing.T, limit int) (*ResponsesWSManager, ResponsesWSOptions, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	handshakes, requests := &atomic.Int32{}, &atomic.Int32{}
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			requests.Add(1)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
				return
			}
		}
	})
	manager := NewResponsesWSManager(context.Background(), limit)
	t.Cleanup(func() { _ = manager.Close() })
	return manager, responsesWSTestOptions(server.URL), handshakes, requests
}

func completeResponsesWSLease(t *testing.T, lease *ResponsesWSLease) {
	t.Helper()
	conn := lease.Conn()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(t.Context(), []byte(`{"type":"response.create"}`)))
	_, err := conn.ReceiveRaw(t.Context())
	require.NoError(t, err)
	require.NoError(t, conn.MarkCompleted())
	lease.Release()
}

func TestResponsesWSManagerSequentialReuseAndCapacity(t *testing.T) {
	m, options, handshakes, requests := responsesWSManagerFixture(t, 1)
	first, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	_, err = m.Acquire(t.Context(), options, 0)
	require.Error(t, err)
	other := options
	other.Owner.ConversationID = "other"
	_, err = m.Acquire(t.Context(), other, 0)
	require.Error(t, err)
	completeResponsesWSLease(t, first)
	second, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.Same(t, first.Conn(), second.Conn())
	completeResponsesWSLease(t, second)
	require.EqualValues(t, 1, handshakes.Load())
	require.EqualValues(t, 2, requests.Load())
	third, err := m.Acquire(t.Context(), other, 0)
	require.NoError(t, err)
	require.NotSame(t, first.Conn(), third.Conn())
	completeResponsesWSLease(t, third)
	require.EqualValues(t, 2, handshakes.Load())
}

func TestResponsesWSManagerTwentyMinuteBoundaryAndActiveLease(t *testing.T) {
	m, options, handshakes, requests := responsesWSManagerFixture(t, 2)
	now := time.Now()
	m.mu.Lock()
	m.now = func() time.Time { return now }
	m.mu.Unlock()
	first, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	completeResponsesWSLease(t, first)
	m.mu.Lock()
	now = now.Add(ResponsesWSPlannedMaxAge - time.Nanosecond)
	// A recently used idle connection isolates the age boundary from idle expiry.
	first.entry.used = now
	m.mu.Unlock()
	before, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.Same(t, first.Conn(), before.Conn())
	m.mu.Lock()
	now = now.Add(time.Nanosecond)
	m.mu.Unlock()
	// Acquisition's expiry scan must not close an active answer at 20 minutes.
	other := options
	other.Owner.ConversationID = "other"
	parallel, err := m.Acquire(t.Context(), other, 0)
	require.NoError(t, err)
	completeResponsesWSLease(t, before)
	parallel.Release()
	at, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NotSame(t, first.Conn(), at.Conn())
	completeResponsesWSLease(t, at)
	require.EqualValues(t, 3, handshakes.Load())
	require.EqualValues(t, 3, requests.Load())
}

func TestResponsesWSManagerCredentialNetworkAndConversationIsolation(t *testing.T) {
	m, options, handshakes, requests := responsesWSManagerFixture(t, 4)
	first, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	completeResponsesWSLease(t, first)
	options.Owner.AuthRevision = "rotated"
	options.AuthHeaders.Set("Authorization", "Bearer rotated-test-secret")
	rotated, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NotSame(t, first.Conn(), rotated.Conn())
	completeResponsesWSLease(t, rotated)
	target := strings.TrimPrefix(options.Endpoint, "ws://")
	proxy := newResponsesWSTestProxy(t, map[string]string{target: target})
	options.Network = ResolveNetworkConfig(nil, &config.NetworkConfig{Proxy: "http://" + proxy.addr()})
	network, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NotSame(t, rotated.Conn(), network.Conn())
	completeResponsesWSLease(t, network)
	options.Owner.ConversationID = "child-session"
	child, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NotSame(t, network.Conn(), child.Conn())
	completeResponsesWSLease(t, child)
	m.InvalidateConversation("child-session")
	require.False(t, child.Conn().Reusable())
	require.True(t, network.Conn().Reusable())
	require.EqualValues(t, 4, handshakes.Load())
	require.EqualValues(t, 4, requests.Load())
	require.Equal(t, []string{target, target}, proxy.authorities(), "both isolated network-policy connections must traverse the real proxy")
}

func TestResponsesWSManagerRejectsUnresolvedProxyNoneBeforeDial(t *testing.T) {
	m, options, handshakes, requests := responsesWSManagerFixture(t, 1)
	options.Network = config.NetworkConfig{Proxy: config.ProxyNone}
	_, err := m.Acquire(t.Context(), options, 0)
	require.Error(t, err)
	require.Zero(t, handshakes.Load())
	require.Zero(t, requests.Load())
	options.Network = ResolveNetworkConfig(nil, &options.Network)
	require.Empty(t, options.Network.Proxy)
	lease, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	completeResponsesWSLease(t, lease)
	require.EqualValues(t, 1, handshakes.Load())
	require.EqualValues(t, 1, requests.Load())
}

func TestResponsesWSManagerCapabilityTTLDoesNotCacheAuthRateOrServerErrors(t *testing.T) {
	for _, status := range []int{404, 405, 426, 401, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(status) })
			m := NewResponsesWSManager(t.Context(), 2)
			defer m.Close()
			now := time.Now()
			m.mu.Lock()
			m.now = func() time.Time { return now }
			m.mu.Unlock()
			options := responsesWSTestOptions(server.URL)
			for range 2 {
				_, err := m.Acquire(t.Context(), options, 0)
				require.Error(t, err)
			}
			if status == 404 || status == 405 || status == 426 {
				require.EqualValues(t, 1, attempts.Load())
			} else {
				require.EqualValues(t, 2, attempts.Load())
			}
			m.mu.Lock()
			now = now.Add(5 * time.Minute)
			m.mu.Unlock()
			_, err := m.Acquire(t.Context(), options, 0)
			require.Error(t, err)
			if status == 404 || status == 405 || status == 426 {
				require.EqualValues(t, 2, attempts.Load())
			} else {
				require.EqualValues(t, 3, attempts.Load())
			}
		})
	}
}

func TestResponsesWSManagerSweeperRetiresIdleAndRevokedWithoutAcquire(t *testing.T) {
	m, options, _, _ := responsesWSManagerFixture(t, 2)
	var current atomic.Bool
	current.Store(true)
	options.StillCurrent = current.Load
	lease, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	completeResponsesWSLease(t, lease)
	m.mu.Lock()
	m.sweepLocked(lease.entry.used.Add(90*time.Second - time.Nanosecond))
	m.mu.Unlock()
	require.True(t, lease.Conn().Reusable())
	m.mu.Lock()
	m.sweepLocked(lease.entry.used.Add(90 * time.Second))
	m.mu.Unlock()
	require.False(t, lease.Conn().Reusable())
	active, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NoError(t, active.Conn().BeginExchange())
	current.Store(false)
	m.mu.Lock()
	m.sweepLocked(m.now())
	m.mu.Unlock()
	require.Error(t, active.Conn().SendRaw(t.Context(), []byte(`{"type":"response.create"}`)))
	active.Release()
	require.NoError(t, m.Close())
	select {
	case <-m.done:
	default:
		t.Fatal("owner sweeper outlived Close")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Empty(t, m.entries)
	require.Empty(t, m.unsupported)
	require.Zero(t, m.occupancy)
}

func TestResponsesWSManagerTLSUsesCloneWithoutPoolingMutablePolicy(t *testing.T) {
	var handshakes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	m := NewResponsesWSManager(t.Context(), 2)
	defer m.Close()
	options := responsesWSTestOptions(server.URL)
	options.Endpoint = strings.Replace(server.URL, "https://", "wss://", 1)
	options.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
	beforeName, beforeRoots := options.TLSClientConfig.ServerName, options.TLSClientConfig.RootCAs
	for range 2 {
		lease, err := m.Acquire(t.Context(), options, 0)
		require.NoError(t, err)
		completeResponsesWSLease(t, lease)
		require.False(t, lease.Conn().Reusable())
	}
	require.Equal(t, beforeName, options.TLSClientConfig.ServerName)
	require.Same(t, beforeRoots, options.TLSClientConfig.RootCAs)
	require.EqualValues(t, 2, handshakes.Load())
}

func TestResponsesWSManagerPendingInvalidationRetainsCapacityUntilDialReturns(t *testing.T) {
	entered := make(chan struct{})
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := NewResponsesWSManager(parent, 1)
	defer m.Close()
	options := responsesWSTestOptions(server.URL)
	done := make(chan error, 1)
	go func() { _, err := m.Acquire(t.Context(), options, 0); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not reach peer")
	}
	m.mu.Lock()
	for key, entry := range m.entries {
		m.removeLocked(key, entry)
	}
	retained := m.occupancy
	m.mu.Unlock()
	require.Equal(t, 1, retained)
	cancel()
	require.NoError(t, m.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("joined handshake did not return")
	}
	_, err := m.Acquire(t.Context(), options, 0)
	require.Error(t, err)
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Zero(t, m.occupancy)
}

func TestResponsesWSManagerShutdownClosesActiveConnection(t *testing.T) {
	m, options, handshakes, requests := responsesWSManagerFixture(t, 1)
	lease, err := m.Acquire(t.Context(), options, 0)
	require.NoError(t, err)
	require.NoError(t, lease.Conn().BeginExchange())
	require.NoError(t, lease.Conn().SendRaw(t.Context(), []byte(`{"type":"response.create"}`)))
	_, err = lease.Conn().ReceiveRaw(t.Context())
	require.NoError(t, err)
	require.NoError(t, m.Close())
	_, err = lease.Conn().ReceiveRaw(t.Context())
	var terminal *ResponsesWSError
	require.ErrorAs(t, err, &terminal)
	require.True(t, terminal.NoReplay())
	lease.Release()
	_, err = m.Acquire(t.Context(), options, 0)
	require.Error(t, err)
	require.EqualValues(t, 1, handshakes.Load())
	require.EqualValues(t, 1, requests.Load())
}
