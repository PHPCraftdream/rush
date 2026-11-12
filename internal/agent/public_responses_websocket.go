package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/PHPCraftdream/rush/internal/session"
)

// The bridge retains fantasy v0.45.5's serializer, event parser and usage logic.
// No HTTP transport is modified; unsupported calls use the current HTTP client.
type publicResponsesPolicy struct {
	transport config.ResponsesTransport
	network   config.NetworkConfig
	apiKey    string
	headers   map[string]string
	client    *http.Client
	revision  uint64
}

type publicResponsesClient struct {
	manager         *nettransport.ResponsesWSManager
	current         func(context.Context) (publicResponsesPolicy, error)
	originalHeaders map[string]string
	valid           func(publicResponsesPolicy) bool
	// Test-only endpoint; production eligibility is restricted to api.openai.com.
	endpoint string
}

type (
	publicResponsesAttemptKey struct{}
	publicResponsesAttempt    struct {
		sent      atomic.Bool
		mu        sync.Mutex
		body      *publicResponsesWSBody
		stop      func() bool
		resolved  bool
		stream    bool
		overrides map[string]bool
	}
)

func newPublicResponsesAttempt(headers map[string]string, userAgent string, stream bool) *publicResponsesAttempt {
	a := &publicResponsesAttempt{stream: stream, overrides: make(map[string]bool)}
	for key := range headers {
		a.overrides[http.CanonicalHeaderKey(key)] = true
	}
	if userAgent != "" {
		a.overrides["User-Agent"] = true
	}
	return a
}

func (a *publicResponsesAttempt) own(ctx context.Context, body *publicResponsesWSBody) {
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		_ = body.conn.Close()
		body.lease.Release()
		return
	}
	body.managed.Store(true)
	a.body = body
	a.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { a.resolve(false) })
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		stop()
		return
	}
	a.stop = stop
	a.mu.Unlock()
}

func (a *publicResponsesAttempt) resolve(accepted bool) {
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		return
	}
	a.resolved = true
	body, stop := a.body, a.stop
	a.body, a.stop = nil, nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	if body != nil {
		if !accepted {
			_ = body.conn.Close()
		}
		body.resolve(accepted)
	}
}

func (a *publicResponsesAttempt) close() { a.resolve(false) }

func newPublicResponsesSDKProvider(baseURL, apiKey string, headers map[string]string, client *publicResponsesClient) (fantasy.Provider, error) {
	opts := []openai.Option{openai.WithAPIKey(apiKey), openai.WithHeaders(headers), openai.WithUseResponsesAPI(), openai.WithHTTPClient(client)}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

type publicResponsesProvider struct{ fantasy.Provider }

func (p *publicResponsesProvider) LanguageModel(ctx context.Context, id string) (fantasy.LanguageModel, error) {
	model, err := p.Provider.LanguageModel(ctx, id)
	if err != nil {
		return nil, err
	}
	return &publicResponsesModel{LanguageModel: model}, nil
}

type publicResponsesModel struct{ fantasy.LanguageModel }

func (m *publicResponsesModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	attempt := newPublicResponsesAttempt(call.Headers, call.UserAgent, false)
	return m.LanguageModel.Generate(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), call)
}

func (m *publicResponsesModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	attempt := newPublicResponsesAttempt(call.Headers, call.UserAgent, false)
	return m.LanguageModel.GenerateObject(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), call)
}

func (m *publicResponsesModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	attempt := newPublicResponsesAttempt(call.Headers, call.UserAgent, true)
	stream, err := m.LanguageModel.Stream(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), call)
	if err != nil {
		attempt.close()
		return nil, guardPublicResponsesError(attempt, err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		accepted := false
		defer func() { attempt.resolve(accepted && ctx.Err() == nil) }()
		finished := false
		for part := range stream {
			if finished {
				return
			}
			if part.Type == fantasy.StreamPartTypeError {
				part.Error = guardPublicResponsesError(attempt, part.Error)
			}
			if part.Type == fantasy.StreamPartTypeFinish {
				finished = true
			}
			if !yield(part) {
				return
			}
			if part.Type == fantasy.StreamPartTypeError {
				return
			}
		}
		accepted = finished
	}, nil
}

func (m *publicResponsesModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	attempt := newPublicResponsesAttempt(call.Headers, call.UserAgent, true)
	stream, err := m.LanguageModel.StreamObject(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), call)
	if err != nil {
		attempt.close()
		return nil, guardPublicResponsesError(attempt, err)
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		accepted := false
		defer func() { attempt.resolve(accepted && ctx.Err() == nil) }()
		finished := false
		for part := range stream {
			if finished {
				return
			}
			if part.Type == fantasy.ObjectStreamPartTypeError {
				part.Error = guardPublicResponsesError(attempt, part.Error)
			}
			if part.Type == fantasy.ObjectStreamPartTypeFinish {
				finished = true
			}
			if !yield(part) {
				return
			}
			if part.Type == fantasy.ObjectStreamPartTypeError {
				return
			}
		}
		accepted = finished
	}, nil
}

func guardPublicResponsesError(attempt *publicResponsesAttempt, err error) error {
	if err != nil && attempt.sent.Load() {
		return nettransport.ResponsesWSNoReplay(err)
	}
	return err
}

func (c *publicResponsesClient) Do(original *http.Request) (*http.Response, error) {
	conversation, _ := original.Context().Value(cliprovider.SessionIDContextKey).(string)
	policy, err := c.current(original.Context())
	if err != nil {
		c.manager.InvalidateConversation(conversation)
		return nil, err
	}
	attempt, _ := original.Context().Value(publicResponsesAttemptKey{}).(*publicResponsesAttempt)
	request := original.Clone(original.Context())
	request.Header = c.requestHeaders(original.Header, policy, attempt)
	fallback := func(reason string) (*http.Response, error) {
		logPublicResponsesFallback(conversation, reason)
		if c.manager.Closed() {
			return nil, errors.New("responses websocket: owner closed")
		}
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		latest, err := c.current(request.Context())
		if err != nil {
			c.manager.InvalidateConversation(conversation)
			return nil, err
		}
		request.Header = c.requestHeaders(original.Header, latest, attempt)
		if latest.client == nil {
			return http.DefaultClient.Do(request)
		}
		return latest.client.Do(request)
	}
	if policy.transport == config.ResponsesTransportHTTP {
		c.manager.InvalidateConversation(conversation)
		return fallback("policy_http")
	}
	if attempt == nil || !attempt.stream || conversation == "" || request.Context().Value(cliprovider.TitleGenerationContextKey) == true || request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "api.openai.com" || request.URL.Path != "/v1/responses" || request.URL.RawQuery != "" {
		return fallback("ineligible_request")
	}
	if request.Body == nil {
		return fallback("ineligible_request")
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, (8<<20)+1))
	if err != nil {
		_ = request.Body.Close()
		return nil, err
	}
	if len(payload) > 8<<20 {
		// Preserve unread bytes for the unchanged HTTP path, without buffering them.
		request.Body = &publicResponsesJoinedBody{Reader: io.MultiReader(bytes.NewReader(payload), request.Body), closer: request.Body}
		return fallback("frame_too_large")
	}
	_ = request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(payload))
	frame, err := nettransport.ResponsesWSFullFrame(payload)
	if err != nil {
		return fallback("frame_encode")
	}
	headers := request.Header.Clone()
	auth := headers.Get("Authorization")
	if auth == "" {
		return fallback("no_authorization")
	}
	headers.Del("Authorization")
	for _, key := range []string{"Content-Type", "Content-Length", "Accept", "Connection", "Upgrade"} {
		headers.Del(key)
	}
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = "wss://api.openai.com/v1/responses"
	}
	revision := sha256.Sum256([]byte(auth))
	owner := hex.EncodeToString(revision[:])
	// open leases a connection and sends the frame. Chain recovery runs it a
	// second time, on a fresh connection, so its first frame is a full one.
	open := func() (*nettransport.ResponsesWSLease, *nettransport.ResponsesWSConn, string, error) {
		lease, err := c.manager.Acquire(request.Context(), nettransport.ResponsesWSOptions{
			Protocol: "public-openai-responses", Endpoint: endpoint, ReadPolicy: nettransport.ResponsesWSReadRequestContext,
			AuthHeaders: http.Header{"Authorization": []string{auth}}, Headers: headers, Network: policy.network,
			StillCurrent: func() bool { return c.valid == nil || c.valid(policy) },
			Owner:        nettransport.ResponsesWSOwner{AccountID: owner, AuthRevision: owner + ":" + strconv.FormatUint(policy.revision, 10), ConversationID: conversation},
		}, nettransport.ResponsesWSPlannedMaxAge)
		if err != nil {
			slog.Debug("responses transport: websocket unavailable, using http", "provider", "openai", "reason", "acquire_failed", "error", err.Error())
			return nil, nil, "acquire_failed", nil
		}
		conn := lease.Conn()
		latest, policyErr := c.current(request.Context())
		if policyErr != nil || latest.revision != policy.revision || latest.apiKey != policy.apiKey || latest.transport != policy.transport || latest.network != policy.network || !maps.Equal(latest.headers, policy.headers) {
			_ = conn.Close()
			lease.Release()
			return nil, nil, "credentials_or_policy_changed", nil
		}
		if err = conn.BeginExchange(); err != nil {
			_ = conn.Close()
			lease.Release()
			return nil, nil, "exchange_state", nil
		}
		if err = conn.SendRaw(request.Context(), frame); err != nil {
			lease.Release()
			var send *nettransport.ResponsesWSError
			if errors.As(err, &send) && send.RetrySafe() {
				return nil, nil, "send_not_sent", nil
			}
			attempt.sent.Store(true)
			return nil, nil, "", err
		}
		attempt.sent.Store(true)
		return lease, conn, "", nil
	}
	lease, conn, reason, err := open()
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return fallback(reason)
	}
	var first []byte
	hasFirst, recovered := false, false
	if conn.Chained() {
		// A chained frame's unknown parent is rejected before any generation.
		// Read the first event here so that rejection is recovered with a full
		// frame instead of reaching the SDK as a failed turn.
		first, err = conn.ReceiveRaw(request.Context())
		hasFirst = true
		if err != nil {
			lease.Release()
			if ctxErr := request.Context().Err(); ctxErr != nil {
				return nil, nettransport.ResponsesWSNoReplay(ctxErr)
			}
			return nil, err
		}
		if nettransport.ResponsesWSChainRejected(first) {
			slog.Debug("responses websocket chain rejected before output, retrying with a full frame", "provider", "openai", "session_hash", session.HashID(conversation))
			_ = conn.Close()
			lease.Release()
			attempt.sent.Store(false)
			first, hasFirst, recovered = nil, false, true
			if lease, conn, reason, err = open(); err != nil {
				return nil, err
			}
			if reason != "" {
				return fallback(reason)
			}
		}
	}
	logPublicResponsesExchange(conversation, conn, recovered)
	body := &publicResponsesWSBody{ctx: request.Context(), lease: lease, conn: conn, first: first, hasFirst: hasFirst}
	attempt.own(request.Context(), body)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: request}, nil
}

// logPublicResponsesFallback and logPublicResponsesExchange emit the same
// transport fields the Codex provider reports, so turns can be compared.
func logPublicResponsesFallback(conversation, reason string) {
	slog.Debug("responses transport exchange", "provider", "openai", "session_hash", session.HashID(conversation), "transport", "http", "http_fallback_reason", reason)
}

func logPublicResponsesExchange(conversation string, conn *nettransport.ResponsesWSConn, recovered bool) {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	recoveries := 0
	if recovered {
		recoveries = 1
	}
	attrs := []any{"provider", "openai", "session_hash", session.HashID(conversation), "transport", "websocket", "chain_recoveries", recoveries}
	slog.Debug("responses transport exchange", append(attrs, conn.FrameAttrs()...)...)
}

func (c *publicResponsesClient) requestHeaders(original http.Header, policy publicResponsesPolicy, attempt *publicResponsesAttempt) http.Header {
	headers := original.Clone()
	protected := func(key string) bool { return attempt != nil && attempt.overrides[http.CanonicalHeaderKey(key)] }
	for key := range c.originalHeaders {
		if !protected(key) {
			headers.Del(key)
		}
	}
	if !protected("Authorization") {
		headers.Set("Authorization", "Bearer "+policy.apiKey)
	}
	for key, value := range policy.headers {
		if protected(key) {
			continue
		}
		headers.Del(key)
		if value != "" {
			headers.Set(key, value)
		}
	}
	return headers
}

type publicResponsesJoinedBody struct {
	io.Reader
	closer io.Closer
}

func (b *publicResponsesJoinedBody) Close() error { return b.closer.Close() }

// Read performs one bounded frame read at a time: no producer goroutine and no
// unbounded queue. The SDK's consumer directly supplies backpressure.
type publicResponsesWSBody struct {
	ctx       context.Context
	lease     *nettransport.ResponsesWSLease
	conn      *nettransport.ResponsesWSConn
	mu        sync.Mutex
	buffer    []byte
	first     []byte // event already read by Do, consumed before the wire
	hasFirst  bool
	terminal  bool
	completed bool
	resolved  bool
	managed   atomic.Bool
	closed    atomic.Bool
}

func (b *publicResponsesWSBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if b.closed.Load() {
		if err := b.ctx.Err(); err != nil {
			return 0, nettransport.ResponsesWSNoReplay(err)
		}
		return 0, io.ErrClosedPipe
	}
	if len(b.buffer) == 0 {
		if b.terminal {
			return 0, io.EOF
		}
		raw := b.first
		hadFirst := b.hasFirst
		b.first, b.hasFirst = nil, false
		if !hadFirst {
			var err error
			raw, err = b.conn.ReceiveRaw(b.ctx)
			if err != nil {
				if ctxErr := b.ctx.Err(); ctxErr != nil {
					return 0, nettransport.ResponsesWSNoReplay(ctxErr)
				}
				return 0, err
			}
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(raw, &event) != nil || event.Type == "" || strings.ContainsAny(event.Type, "\r\n") {
			return 0, b.conn.Abort(errors.New("invalid Responses event"))
		}
		switch event.Type {
		case "error", "response.failed":
			return 0, b.conn.Abort(errors.New("responses server rejected exchange"))
		case "response.completed":
			if event.Response.Status != "completed" {
				return 0, b.conn.Abort(errors.New("invalid Responses completion"))
			}
			b.completed = true
			b.terminal = true
		case "response.incomplete":
			if event.Response.Status != "incomplete" {
				return 0, b.conn.Abort(errors.New("invalid Responses incomplete status"))
			}
			b.terminal = true
			_ = b.conn.Close()
		}
		b.buffer = append([]byte("event: "+event.Type+"\ndata: "), raw...)
		b.buffer = append(b.buffer, '\n', '\n')
	}
	n := copy(p, b.buffer)
	b.buffer = b.buffer[n:]
	return n, nil
}

func (b *publicResponsesWSBody) Close() error {
	b.closed.Store(true)
	if b.managed.Load() {
		return nil
	}
	// An unmanaged Close abandons the exchange. Close the wire before acquiring
	// the parser lock because Read may be blocked in ReceiveRaw.
	_ = b.conn.Close()
	b.mu.Lock()
	if b.resolved {
		b.mu.Unlock()
		return nil
	}
	b.resolved = true
	b.mu.Unlock()
	b.lease.Release()
	return nil
}

func (b *publicResponsesWSBody) resolve(accepted bool) {
	b.mu.Lock()
	if b.resolved {
		b.mu.Unlock()
		return
	}
	accepted = accepted && b.completed && b.terminal && len(b.buffer) == 0 && b.ctx.Err() == nil
	b.resolved = true
	b.mu.Unlock()
	b.finish(accepted)
}

func (b *publicResponsesWSBody) finish(accepted bool) {
	b.mu.Lock()
	completed := b.completed && b.terminal && len(b.buffer) == 0 && b.ctx.Err() == nil
	b.mu.Unlock()
	if accepted && completed {
		if err := b.conn.MarkCompleted(); err == nil {
			b.lease.Release()
			return
		}
	}
	_ = b.conn.Close()
	b.lease.Release()
}
