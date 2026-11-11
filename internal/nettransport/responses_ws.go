package nettransport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
)

const (
	responsesWSDefaultHandshakeTimeout = 20 * time.Second
	responsesWSMaxHandshakeTimeout     = 45 * time.Second
	responsesWSDefaultIOTimeout        = 30 * time.Second
	responsesWSMaxIOTimeout            = 60 * time.Second
	responsesWSDefaultFrameBytes       = 8 << 20
	responsesWSMaxFrameBytes           = 16 << 20
	responsesWSDefaultExchangeBytes    = 32 << 20
	responsesWSMaxExchangeBytes        = 64 << 20
)

type ResponsesWSSendState uint8

const (
	ResponsesWSSendNotSent ResponsesWSSendState = iota
	ResponsesWSSendPossiblySent
	ResponsesWSStreaming
	ResponsesWSCompleted
)

type ResponsesWSReadPolicy uint8

const (
	ResponsesWSReadBounded ResponsesWSReadPolicy = iota
	// ResponsesWSReadRequestContext delegates idle/hard-stop policy to the
	// caller's existing stream watchdog; owner shutdown still closes the wire.
	ResponsesWSReadRequestContext
)

type ResponsesWSOwner struct {
	AccountID      string
	AuthRevision   string
	ConversationID string
}

type ResponsesWSOptions struct {
	Protocol         string
	Endpoint         string
	AuthToken        string
	AuthHeaders      http.Header
	Headers          http.Header
	TLSClientConfig  *tls.Config
	Owner            ResponsesWSOwner
	Network          config.NetworkConfig
	HandshakeTimeout time.Duration
	IOTimeout        time.Duration
	ReadPolicy       ResponsesWSReadPolicy
	MaxFrameBytes    int64
	MaxExchangeBytes int64
	// StillCurrent is a non-blocking, memory-only ownership check.
	StillCurrent func() bool
}

type ResponsesWSHandshakeError struct {
	Status int
}

func (e *ResponsesWSHandshakeError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("responses websocket: handshake rejected with HTTP status %d", e.Status)
	}
	return "responses websocket: handshake failed"
}

type ResponsesWSError struct {
	state ResponsesWSSendState
	stage string
	cause error
}

func (e *ResponsesWSError) Error() string {
	if errors.Is(e.cause, context.Canceled) {
		return fmt.Sprintf("responses websocket: %s canceled (%s)", e.stage, e.state)
	}
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return fmt.Sprintf("responses websocket: %s deadline exceeded (%s)", e.stage, e.state)
	}
	return fmt.Sprintf("responses websocket: %s failed (%s)", e.stage, e.state)
}

// ResponsesWSNoReplay hides provider/network retry classifications after send.
func ResponsesWSNoReplay(cause error) error {
	return &ResponsesWSError{state: ResponsesWSSendPossiblySent, stage: "response", cause: cause}
}

func (e *ResponsesWSError) State() ResponsesWSSendState { return e.state }
func (e *ResponsesWSError) RetrySafe() bool             { return e.state == ResponsesWSSendNotSent }
func (e *ResponsesWSError) NoReplay() bool              { return e.state != ResponsesWSSendNotSent }
func (e *ResponsesWSError) Is(target error) bool {
	return (target == context.Canceled && errors.Is(e.cause, context.Canceled)) ||
		(target == context.DeadlineExceeded && errors.Is(e.cause, context.DeadlineExceeded))
}

func (s ResponsesWSSendState) String() string {
	switch s {
	case ResponsesWSSendNotSent:
		return "not_sent"
	case ResponsesWSSendPossiblySent:
		return "possibly_sent"
	case ResponsesWSStreaming:
		return "streaming"
	case ResponsesWSCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

type responsesWSWire interface {
	WriteMessage(int, []byte) error
	ReadMessage() (int, []byte, error)
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetReadLimit(int64)
	Close() error
}

type ResponsesWSConn struct {
	wire             responsesWSWire
	owner            ResponsesWSOwner
	endpoint         string
	protocol         string
	ioTimeout        time.Duration
	readPolicy       ResponsesWSReadPolicy
	maxFrameBytes    int64
	maxExchangeBytes int64
	stateMu          sync.Mutex
	deltaMu          sync.Mutex
	delta            responsesWSDeltaState
	deltaEnabled     bool
	opMu             sync.Mutex
	state            ResponsesWSSendState
	exchangeBytes    int64
	active           bool
	hasCompleted     bool
	closed           bool
	closeOnce        sync.Once
}

func DialResponsesWebSocket(ctx context.Context, options ResponsesWSOptions) (*ResponsesWSConn, error) {
	if ctx == nil {
		return nil, errors.New("responses websocket: nil context")
	}
	if err := validateResponsesWSOptions(&options); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") {
		return nil, errors.New("responses websocket: invalid endpoint")
	}

	var proxyDial dialFunc
	resolvedConfig, err := resolveConfig(options.Network)
	if err != nil {
		return nil, errors.New("responses websocket: invalid network policy")
	}
	if resolvedConfig.empty() {
		requestScheme := "http"
		if endpoint.Scheme == "wss" {
			requestScheme = "https"
		}
		request := &http.Request{URL: &url.URL{Scheme: requestScheme, Host: endpoint.Host}}
		proxyURL, proxyErr := http.ProxyFromEnvironment(request)
		if proxyErr != nil {
			return nil, errors.New("responses websocket: invalid environment proxy policy")
		}
		if proxyURL != nil {
			if proxyURL.Scheme != "http" && proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h" {
				return nil, errors.New("responses websocket: unsupported environment proxy policy")
			}
			proxyDial, err = proxyDialer(proxyURL)
			if err != nil {
				return nil, errors.New("responses websocket: invalid environment proxy policy")
			}
		}
	}
	if resolvedConfig.proxyURL != nil {
		proxyDial, err = proxyDialer(resolvedConfig.proxyURL)
		if err != nil {
			return nil, errors.New("responses websocket: invalid network policy")
		}
	}
	resolver, resolverTransports, err := buildResolver(resolvedConfig, proxyDial)
	if err != nil {
		return nil, errors.New("responses websocket: unable to build network policy")
	}
	defer func() {
		for _, transport := range resolverTransports {
			transport.CloseIdleConnections()
		}
	}()

	var dial dialFunc
	switch {
	case resolver != nil:
		dial = resolvedDialer(resolver, proxyDial)
	case proxyDial != nil:
		dial = proxyDial
	default:
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: transportDialTimeout}).DialContext(ctx, network, address)
		}
	}

	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, options.HandshakeTimeout)
	defer cancelHandshake()
	var guarded *responsesWSCancelConn
	dialer := websocket.Dialer{
		HandshakeTimeout: options.HandshakeTimeout,
		Proxy:            nil,
		TLSClientConfig:  cloneResponsesWSTLS(options.TLSClientConfig),
		NetDialContext: func(dialCtx context.Context, network, address string) (net.Conn, error) {
			conn, dialErr := dial(dialCtx, network, address)
			if dialErr != nil {
				return nil, dialErr
			}
			guarded = newResponsesWSCancelConn(handshakeCtx, conn)
			return guarded, nil
		},
	}
	header := options.Headers.Clone()
	if header == nil {
		header = make(http.Header)
	}
	for key, values := range options.AuthHeaders.Clone() {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	if options.AuthToken != "" {
		header.Set("Authorization", "Bearer "+options.AuthToken)
	}
	wire, response, dialErr := dialer.DialContext(handshakeCtx, endpoint.String(), header)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if dialErr != nil {
		if guarded != nil {
			_ = guarded.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if handshakeCtx.Err() != nil {
			return nil, context.DeadlineExceeded
		}
		if response != nil {
			return nil, &ResponsesWSHandshakeError{Status: response.StatusCode}
		}
		return nil, &ResponsesWSHandshakeError{}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = wire.Close()
		return nil, ctxErr
	}
	if guarded == nil || !guarded.keep() {
		_ = wire.Close()
		return nil, errors.New("responses websocket: connection canceled during handshake")
	}
	wire.SetReadLimit(options.MaxFrameBytes)
	return &ResponsesWSConn{
		wire:             wire,
		owner:            options.Owner,
		endpoint:         options.Endpoint,
		protocol:         options.Protocol,
		ioTimeout:        options.IOTimeout,
		readPolicy:       options.ReadPolicy,
		maxFrameBytes:    options.MaxFrameBytes,
		maxExchangeBytes: options.MaxExchangeBytes,
		deltaEnabled:     responsesWSDeltaAllowed(options.Protocol, header),
		delta:            responsesWSDeltaState{requireStream: options.Protocol == "codex-responses"},
	}, nil
}

func validateResponsesWSOptions(options *ResponsesWSOptions) error {
	if options.ReadPolicy > ResponsesWSReadRequestContext {
		return errors.New("responses websocket: invalid read policy")
	}
	if options.Protocol == "" || strings.ContainsAny(options.Protocol, "\r\n") || options.Endpoint == "" || options.Owner.AccountID == "" || options.Owner.AuthRevision == "" || options.Owner.ConversationID == "" {
		return errors.New("responses websocket: incomplete connection options")
	}
	if (options.AuthToken == "") == (len(options.AuthHeaders) == 0) {
		return errors.New("responses websocket: exactly one authentication method is required")
	}
	if options.AuthToken != "" && strings.ContainsAny(options.AuthToken, "\r\n") {
		return errors.New("responses websocket: invalid authentication headers")
	}
	if options.AuthHeaders.Get("Proxy-Authorization") != "" {
		return errors.New("responses websocket: invalid authentication headers")
	}
	if options.Headers.Get("Authorization") != "" || options.Headers.Get("Proxy-Authorization") != "" {
		return errors.New("responses websocket: authentication headers must use the auth fields")
	}
	if options.HandshakeTimeout < 0 || options.IOTimeout < 0 || options.MaxFrameBytes < 0 || options.MaxExchangeBytes < 0 {
		return errors.New("responses websocket: invalid connection bounds")
	}
	if options.HandshakeTimeout == 0 {
		options.HandshakeTimeout = responsesWSDefaultHandshakeTimeout
	}
	options.HandshakeTimeout = min(options.HandshakeTimeout, responsesWSMaxHandshakeTimeout)
	if options.IOTimeout == 0 {
		options.IOTimeout = responsesWSDefaultIOTimeout
	}
	options.IOTimeout = min(options.IOTimeout, responsesWSMaxIOTimeout)
	if options.MaxFrameBytes == 0 {
		options.MaxFrameBytes = responsesWSDefaultFrameBytes
	}
	if options.MaxFrameBytes > responsesWSMaxFrameBytes {
		options.MaxFrameBytes = responsesWSMaxFrameBytes
	}
	if options.MaxExchangeBytes == 0 {
		options.MaxExchangeBytes = responsesWSDefaultExchangeBytes
	}
	if options.MaxExchangeBytes > responsesWSMaxExchangeBytes {
		options.MaxExchangeBytes = responsesWSMaxExchangeBytes
	}
	if options.MaxExchangeBytes < options.MaxFrameBytes {
		options.MaxFrameBytes = options.MaxExchangeBytes
	}
	return nil
}

func cloneResponsesWSTLS(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return nil
	}
	return cfg.Clone()
}

func (c *ResponsesWSConn) Owner() ResponsesWSOwner { return c.owner }
func (c *ResponsesWSConn) Endpoint() string        { return c.endpoint }
func (c *ResponsesWSConn) Protocol() string        { return c.protocol }

func (c *ResponsesWSConn) State() ResponsesWSSendState {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.state
}

func (c *ResponsesWSConn) Reusable() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return !c.closed && c.hasCompleted && c.state == ResponsesWSCompleted && !c.active
}

func (c *ResponsesWSConn) BeginExchange() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed || c.active || (c.hasCompleted && c.state != ResponsesWSCompleted) || (!c.hasCompleted && c.state != ResponsesWSSendNotSent) {
		return errors.New("responses websocket: previous exchange is not complete")
	}
	c.state = ResponsesWSSendNotSent
	c.exchangeBytes = 0
	c.active = true
	return nil
}

func (c *ResponsesWSConn) SendRaw(ctx context.Context, frame []byte) error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if ctx == nil {
		return c.fail("send", ResponsesWSSendNotSent, errors.New("invalid context"))
	}
	c.stateMu.Lock()
	state, closed, used := c.state, c.closed, c.exchangeBytes
	c.stateMu.Unlock()
	if closed || state != ResponsesWSSendNotSent || !c.isActive() {
		return c.fail("send", state, errors.New("invalid exchange state"))
	}
	if int64(len(frame)) > c.maxFrameBytes || used+int64(len(frame)) > c.maxExchangeBytes {
		return c.fail("send bounds", ResponsesWSSendNotSent, errors.New("frame exceeds bounds"))
	}
	if err := ctx.Err(); err != nil {
		return c.fail("send", ResponsesWSSendNotSent, err)
	}
	if err := c.wire.SetWriteDeadline(ioDeadline(ctx, c.ioTimeout)); err != nil {
		return c.fail("send", ResponsesWSSendNotSent, err)
	}
	c.deltaMu.Lock()
	wireFrame, _ := c.delta.prepare(frame, c.deltaEnabled)
	c.deltaMu.Unlock()
	stopClose := contextDeadlineFunc(ctx, func() { _ = c.Close() })
	defer stopClose()
	c.stateMu.Lock()
	if c.closed || c.state != ResponsesWSSendNotSent {
		state := c.state
		c.stateMu.Unlock()
		return c.fail("send", state, errors.New("invalid exchange state"))
	}
	c.state = ResponsesWSSendPossiblySent
	c.stateMu.Unlock()
	if err := c.wire.WriteMessage(websocket.TextMessage, wireFrame); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return c.fail("send", ResponsesWSSendPossiblySent, err)
	}
	c.stateMu.Lock()
	c.exchangeBytes += int64(len(wireFrame))
	c.stateMu.Unlock()
	return nil
}

func (c *ResponsesWSConn) ReceiveRaw(ctx context.Context) ([]byte, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if ctx == nil {
		return nil, c.fail("receive", c.State(), errors.New("invalid context"))
	}
	c.stateMu.Lock()
	state, closed := c.state, c.closed
	c.stateMu.Unlock()
	if closed || (state != ResponsesWSSendPossiblySent && state != ResponsesWSStreaming) || !c.isActive() {
		return nil, c.fail("receive", state, errors.New("invalid exchange state"))
	}
	if err := ctx.Err(); err != nil {
		return nil, c.fail("receive", state, err)
	}
	readDeadline := ioDeadline(ctx, c.ioTimeout)
	if c.readPolicy == ResponsesWSReadRequestContext {
		readDeadline, _ = ctx.Deadline()
	}
	if err := c.wire.SetReadDeadline(readDeadline); err != nil {
		return nil, c.fail("receive", state, err)
	}
	stopClose := contextDeadlineFunc(ctx, func() { _ = c.Close() })
	defer stopClose()
	messageType, raw, err := c.wire.ReadMessage()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		return nil, c.fail("receive", c.State(), err)
	}
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return nil, c.fail("receive", c.State(), errors.New("invalid message type"))
	}
	c.stateMu.Lock()
	c.state = ResponsesWSStreaming
	c.exchangeBytes += int64(len(raw))
	overBudget := c.exchangeBytes > c.maxExchangeBytes
	c.stateMu.Unlock()
	if int64(len(raw)) > c.maxFrameBytes || overBudget {
		return nil, c.fail("receive bounds", ResponsesWSStreaming, errors.New("frame exceeds bounds"))
	}
	c.deltaMu.Lock()
	c.delta.observe(raw)
	c.deltaMu.Unlock()
	return raw, nil
}

func (c *ResponsesWSConn) MarkCompleted() error {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed || !c.active || (c.state != ResponsesWSSendPossiblySent && c.state != ResponsesWSStreaming) {
		return errors.New("responses websocket: exchange cannot be completed")
	}
	c.state = ResponsesWSCompleted
	c.active = false
	c.hasCompleted = true
	c.deltaMu.Lock()
	c.delta.commit()
	c.deltaMu.Unlock()
	return nil
}

// Abort retires a protocol-invalid exchange without exposing replayable causes.
func (c *ResponsesWSConn) Abort(cause error) error {
	return c.fail("protocol", c.State(), cause)
}

func (c *ResponsesWSConn) isActive() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.active
}

func (c *ResponsesWSConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		c.stateMu.Unlock()
		c.deltaMu.Lock()
		c.delta.clear()
		c.deltaMu.Unlock()
		closeErr = c.wire.Close()
	})
	return closeErr
}

func (c *ResponsesWSConn) fail(stage string, state ResponsesWSSendState, cause error) error {
	c.stateMu.Lock()
	if c.state > state {
		state = c.state
	}
	c.stateMu.Unlock()
	c.deltaMu.Lock()
	c.delta.clear()
	c.deltaMu.Unlock()
	_ = c.Close()
	closeStatus := 0
	var closeError *websocket.CloseError
	if errors.As(cause, &closeError) {
		closeStatus = closeError.Code
	}
	slog.Debug("responses websocket exchange failed", "protocol", c.protocol, "stage", stage, "send_state", state.String(), "close_status", closeStatus)
	return &ResponsesWSError{state: state, stage: stage, cause: cause}
}

func ioDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		return callerDeadline
	}
	return deadline
}

func contextDeadlineFunc(ctx context.Context, setDeadline func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		setDeadline()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

type responsesWSCancelConn struct {
	net.Conn
	ctx       context.Context
	mu        sync.Mutex
	stop      chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	detached  bool
	aborted   bool
}

func newResponsesWSCancelConn(ctx context.Context, conn net.Conn) *responsesWSCancelConn {
	guarded := &responsesWSCancelConn{Conn: conn, ctx: ctx, stop: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			guarded.mu.Lock()
			if !guarded.detached {
				guarded.aborted = true
				guarded.mu.Unlock()
				guarded.closeUnderlying()
				return
			}
			guarded.mu.Unlock()
		case <-guarded.stop:
		}
	}()
	return guarded
}

func (c *responsesWSCancelConn) keep() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.aborted || c.ctx.Err() != nil {
		c.aborted = true
		return false
	}
	c.detached = true
	c.stopOnce.Do(func() { close(c.stop) })
	return true
}

func (c *responsesWSCancelConn) Close() error {
	c.mu.Lock()
	c.detached = true
	c.stopOnce.Do(func() { close(c.stop) })
	c.mu.Unlock()
	return c.closeUnderlying()
}

func (c *responsesWSCancelConn) closeUnderlying() error {
	var err error
	c.closeOnce.Do(func() { err = c.Conn.Close() })
	return err
}
