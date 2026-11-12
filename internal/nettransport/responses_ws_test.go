package nettransport

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func responsesWSTestOptions(rawURL string) ResponsesWSOptions {
	return ResponsesWSOptions{
		Protocol:    "responses=v1",
		Endpoint:    strings.Replace(rawURL, "http://", "ws://", 1),
		AuthHeaders: http.Header{"Authorization": []string{"Bearer responses-test-secret"}},
		Headers:     http.Header{"OpenAI-Beta": []string{"responses=v1"}},
		Owner:       ResponsesWSOwner{AccountID: "account-test", AuthRevision: "credential-revision-test", ConversationID: "conversation-test"},
	}
}

func responsesWSTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func responsesWSUpgrade(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		t.Errorf("websocket upgrade: %v", err)
		return nil
	}
	return conn
}

func TestResponsesWSDialCancellationUnblocksHandshake(t *testing.T) {
	entered := make(chan struct{})
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := DialResponsesWebSocket(ctx, responsesWSTestOptions(server.URL))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not reach local endpoint")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not return after cancellation")
	}
}

func TestResponsesWSRedirectDoesNotForwardAuthorization(t *testing.T) {
	var targetHits int
	var mu sync.Mutex
	target := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHits++
		mu.Unlock()
		_, _ = w.Write([]byte("redirect target"))
	})
	redirect := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})
	_, err := DialResponsesWebSocket(context.Background(), responsesWSTestOptions(redirect.URL))
	var handshake *ResponsesWSHandshakeError
	require.ErrorAs(t, err, &handshake)
	require.Equal(t, http.StatusFound, handshake.Status)
	mu.Lock()
	require.Zero(t, targetHits)
	mu.Unlock()
}

func TestResponsesWSProxyAndCustomDNSPolicy(t *testing.T) {
	dns := startDNSServer(t, loopbackIP)
	var wsRequests atomic.Int32
	target := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer responses-test-secret", r.Header.Get("Authorization"))
		require.Equal(t, "responses=v1", r.Header.Get("OpenAI-Beta"))
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
		wsRequests.Add(1)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("raw reply")))
	})
	targetAddr := hostOnlyOfURL(t, target.URL)
	targetPort := requirePort(t, targetAddr)
	proxy := newResponsesWSTestProxy(t, map[string]string{
		dns.tcpAddr(): dns.tcpAddr(),
		net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)): targetAddr,
	})
	options := responsesWSTestOptions("http://responses-dns.invalid:" + strconv.Itoa(targetPort) + "/v1/responses")
	options.Network = config.NetworkConfig{
		Proxy:     "http://proxy-user:proxy-pass@" + proxy.addr(),
		DNSServer: dns.tcpAddr(),
	}
	conn, err := DialResponsesWebSocket(context.Background(), options)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(context.Background(), []byte("raw request")))
	reply, err := conn.ReceiveRaw(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte("raw reply"), reply)
	require.NoError(t, conn.MarkCompleted())
	require.True(t, conn.Reusable())
	// A and AAAA each use a separate authenticated DNS tunnel before the WS tunnel.
	require.Equal(t, []string{dns.tcpAddr(), dns.tcpAddr(), net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort))}, proxy.authorities())
	proxyAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-pass"))
	require.Equal(t, []string{proxyAuth, proxyAuth, proxyAuth}, proxy.authValues())
	require.Contains(t, dns.names(), "responses-dns.invalid")
	require.Equal(t, int32(1), wsRequests.Load())
}

func TestResponsesWSBeforeSendCancellationIsRetrySafe(t *testing.T) {
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn != nil {
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
		}
	})
	conn, err := DialResponsesWebSocket(context.Background(), responsesWSTestOptions(server.URL))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = conn.SendRaw(ctx, []byte("not written"))
	var sendErr *ResponsesWSError
	require.ErrorAs(t, err, &sendErr)
	require.Equal(t, ResponsesWSSendNotSent, sendErr.State())
	require.True(t, sendErr.RetrySafe())
	require.False(t, sendErr.NoReplay())
	require.False(t, conn.Reusable())
}

func TestResponsesWSRawSendReceiveStateAndExplicitCompletion(t *testing.T) {
	requests := make(chan []byte, 1)
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		_, request, err := conn.ReadMessage()
		require.NoError(t, err)
		requests <- request
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)))
	})
	conn, err := DialResponsesWebSocket(context.Background(), responsesWSTestOptions(server.URL))
	require.NoError(t, err)
	defer conn.Close()
	require.False(t, conn.Reusable())
	require.NoError(t, conn.BeginExchange())
	frame := []byte(`{"type":"response.create","input":"opaque"}`)
	require.NoError(t, conn.SendRaw(context.Background(), frame))
	require.Equal(t, ResponsesWSSendPossiblySent, conn.State())
	require.Equal(t, frame, <-requests)
	received, err := conn.ReceiveRaw(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte(`{"type":"response.completed"}`), received)
	require.Equal(t, ResponsesWSStreaming, conn.State())
	require.NoError(t, conn.MarkCompleted())
	require.Equal(t, ResponsesWSCompleted, conn.State())
	require.True(t, conn.Reusable())
	require.NoError(t, conn.BeginExchange())
	require.False(t, conn.Reusable())
}

func TestResponsesWSFailedWriteIsAmbiguousAndNeverReplayable(t *testing.T) {
	wire := &responsesWSTestWire{writeErr: errors.New("synthetic partial write")}
	conn := newResponsesWSTestConn(wire, 64, 128)
	require.NoError(t, conn.BeginExchange())
	err := conn.SendRaw(context.Background(), []byte("frame"))
	var sendErr *ResponsesWSError
	require.ErrorAs(t, err, &sendErr)
	require.Equal(t, ResponsesWSSendPossiblySent, sendErr.State())
	require.False(t, sendErr.RetrySafe())
	require.True(t, sendErr.NoReplay())
	require.Equal(t, []byte("frame"), wire.written)
	require.True(t, wire.closed)
}

func TestResponsesWSMidstreamDisconnectIsNoReplay(t *testing.T) {
	ready := make(chan struct{})
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
		close(ready)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("first event")))
		_ = conn.UnderlyingConn().Close()
	})
	conn, err := DialResponsesWebSocket(context.Background(), responsesWSTestOptions(server.URL))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(context.Background(), []byte("request")))
	<-ready
	first, err := conn.ReceiveRaw(context.Background())
	require.NoError(t, err)
	require.Equal(t, []byte("first event"), first)
	_, err = conn.ReceiveRaw(context.Background())
	var receiveErr *ResponsesWSError
	require.ErrorAs(t, err, &receiveErr)
	require.Equal(t, ResponsesWSStreaming, receiveErr.State())
	require.True(t, receiveErr.NoReplay())
	require.False(t, conn.Reusable())
}

func TestResponsesWSReadTimeoutIsBoundedAndNoReplay(t *testing.T) {
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		// Hijacked request contexts do not observe client disconnects; read the socket.
		_, _, _ = conn.ReadMessage()
	})
	options := responsesWSTestOptions(server.URL)
	options.IOTimeout = 100 * time.Millisecond
	conn, err := DialResponsesWebSocket(context.Background(), options)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(context.Background(), []byte("request")))
	started := time.Now()
	_, err = conn.ReceiveRaw(context.Background())
	var receiveErr *ResponsesWSError
	require.ErrorAs(t, err, &receiveErr)
	require.True(t, receiveErr.NoReplay())
	require.Less(t, time.Since(started), 2*time.Second)
	require.False(t, conn.Reusable())
}

func TestResponsesWSFrameAndExchangeBudgets(t *testing.T) {
	t.Run("outbound frame budget is retry-safe before write", func(t *testing.T) {
		wire := &responsesWSTestWire{}
		conn := newResponsesWSTestConn(wire, 4, 8)
		require.NoError(t, conn.BeginExchange())
		err := conn.SendRaw(context.Background(), []byte("12345"))
		var sendErr *ResponsesWSError
		require.ErrorAs(t, err, &sendErr)
		require.True(t, sendErr.RetrySafe())
		require.Empty(t, wire.written)
	})
	t.Run("inbound frame limit closes and forbids replay", func(t *testing.T) {
		server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			conn := responsesWSUpgrade(t, w, r)
			if conn == nil {
				return
			}
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteMessage(websocket.TextMessage, []byte("12345"))
		})
		options := responsesWSTestOptions(server.URL)
		options.MaxFrameBytes = 4
		options.MaxExchangeBytes = 8
		conn, err := DialResponsesWebSocket(context.Background(), options)
		require.NoError(t, err)
		defer conn.Close()
		require.NoError(t, conn.BeginExchange())
		require.NoError(t, conn.SendRaw(context.Background(), []byte("ok")))
		_, err = conn.ReceiveRaw(context.Background())
		var receiveErr *ResponsesWSError
		require.ErrorAs(t, err, &receiveErr)
		require.True(t, receiveErr.NoReplay())
	})
	t.Run("total inbound exchange budget", func(t *testing.T) {
		server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			conn := responsesWSUpgrade(t, w, r)
			if conn == nil {
				return
			}
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteMessage(websocket.TextMessage, []byte("1234"))
			_ = conn.WriteMessage(websocket.TextMessage, []byte("5678"))
		})
		options := responsesWSTestOptions(server.URL)
		options.MaxFrameBytes = 4
		options.MaxExchangeBytes = 6
		conn, err := DialResponsesWebSocket(context.Background(), options)
		require.NoError(t, err)
		defer conn.Close()
		require.NoError(t, conn.BeginExchange())
		require.NoError(t, conn.SendRaw(context.Background(), []byte("x")))
		_, err = conn.ReceiveRaw(context.Background())
		require.NoError(t, err)
		_, err = conn.ReceiveRaw(context.Background())
		var receiveErr *ResponsesWSError
		require.ErrorAs(t, err, &receiveErr)
		require.True(t, receiveErr.NoReplay())
	})
}

func TestResponsesWSAuthAndProtocolHeadersAreAdapterOwned(t *testing.T) {
	auth := http.Header{"Authorization": []string{"Bearer adapter-token"}, "X-Account": []string{"tenant-7"}}
	headers := make(http.Header)
	headers.Set("OpenAI-Beta", "responses=v2")
	options := ResponsesWSOptions{
		Protocol:    "responses-v2-opaque-protocol",
		Endpoint:    "ws://127.0.0.1/v1/responses",
		AuthHeaders: auth,
		Headers:     headers,
		Owner:       ResponsesWSOwner{AccountID: "tenant-7", AuthRevision: "rev-2", ConversationID: "conversation"},
	}
	require.NoError(t, validateResponsesWSOptions(&options))
	require.Equal(t, "Bearer adapter-token", auth.Get("Authorization"))
	require.Equal(t, "responses=v2", headers.Get("OpenAI-Beta"))
	require.Equal(t, "responses=v2", options.Headers.Get("OpenAI-Beta"))
}

func TestResponsesWSOptionsHaveHardUpperBounds(t *testing.T) {
	options := ResponsesWSOptions{
		Protocol:         "responses=v1",
		Endpoint:         "ws://127.0.0.1/",
		AuthToken:        "token",
		Owner:            ResponsesWSOwner{AccountID: "account", AuthRevision: "revision", ConversationID: "conversation"},
		HandshakeTimeout: time.Hour,
		IOTimeout:        time.Hour,
		MaxFrameBytes:    1 << 30,
		MaxExchangeBytes: 1 << 30,
	}
	require.NoError(t, validateResponsesWSOptions(&options))
	require.Equal(t, responsesWSMaxHandshakeTimeout, options.HandshakeTimeout)
	require.Equal(t, responsesWSMaxIOTimeout, options.IOTimeout)
	require.Equal(t, int64(responsesWSMaxFrameBytes), options.MaxFrameBytes)
	require.Equal(t, int64(responsesWSMaxExchangeBytes), options.MaxExchangeBytes)
}

type responsesWSTestWire struct {
	writeErr error
	written  []byte
	closed   bool
	read     [][]byte
}

func (w *responsesWSTestWire) WriteMessage(_ int, data []byte) error {
	w.written = append(w.written, data...)
	return w.writeErr
}

func (w *responsesWSTestWire) ReadMessage() (int, []byte, error) {
	if len(w.read) == 0 {
		return 0, nil, io.EOF
	}
	message := w.read[0]
	w.read = w.read[1:]
	return websocket.TextMessage, message, nil
}
func (*responsesWSTestWire) SetWriteDeadline(time.Time) error { return nil }
func (*responsesWSTestWire) SetReadDeadline(time.Time) error  { return nil }
func (*responsesWSTestWire) SetReadLimit(int64)               {}
func (w *responsesWSTestWire) Close() error {
	w.closed = true
	return nil
}

func newResponsesWSTestConn(wire responsesWSWire, frameLimit, exchangeLimit int64) *ResponsesWSConn {
	return &ResponsesWSConn{
		wire:             wire,
		ioTimeout:        time.Second,
		maxFrameBytes:    frameLimit,
		maxExchangeBytes: exchangeLimit,
		state:            ResponsesWSSendNotSent,
	}
}

type responsesWSTestProxy struct {
	ctx          context.Context
	listener     net.Listener
	mu           sync.Mutex
	routes       map[string]string
	authority    []string
	recordedAuth []string
}

func newResponsesWSTestProxy(t *testing.T, routes map[string]string) *responsesWSTestProxy {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &responsesWSTestProxy{ctx: t.Context(), listener: listener, routes: routes}
	t.Cleanup(func() { _ = listener.Close() })
	go proxy.serve()
	return proxy
}

func (p *responsesWSTestProxy) addr() string { return p.listener.Addr().String() }

func (p *responsesWSTestProxy) authorities() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.authority...)
}

func (p *responsesWSTestProxy) authValues() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.recordedAuth...)
}

func (p *responsesWSTestProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *responsesWSTestProxy) handle(client net.Conn) {
	defer client.Close()
	reader := bufio.NewReader(client)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if request.Method != http.MethodConnect {
		_, _ = io.WriteString(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	p.mu.Lock()
	p.authority = append(p.authority, request.Host)
	p.recordedAuth = append(p.recordedAuth, request.Header.Get("Proxy-Authorization"))
	target := p.routes[request.Host]
	p.mu.Unlock()
	if target == "" {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	upstream, err := (&net.Dialer{Timeout: time.Second}).DialContext(p.ctx, "tcp", target)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, reader)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
}
