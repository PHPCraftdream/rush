package codexprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type WebSocketTransport string

const (
	WebSocketTransportAuto WebSocketTransport = "auto"
	WebSocketTransportHTTP WebSocketTransport = "http"
)

// WebSocketPolicy contains opaque ownership identity, not config dependencies.
type WebSocketPolicy struct {
	Transport       WebSocketTransport
	NetworkIdentity string
	Revision        uint64
}

type WebSocketExchangeError interface {
	error
	RetrySafe() bool
	NoReplay() bool
}

type WebSocketConnection interface {
	BeginExchange() error
	SendRaw(context.Context, []byte) error
	ReceiveRaw(context.Context) ([]byte, error)
	MarkCompleted() error
	Abort(error) error
	Close() error
}

type WebSocketLease interface {
	Conn() WebSocketConnection
	Release()
}

type WebSocketManager interface {
	Closed() bool
	InvalidateConversation(string)
	Acquire(context.Context, WebSocketRequest) (WebSocketLease, error)
	UnauthorizedHandshake(error) bool
}

type WebSocketRequest struct {
	Endpoint, AuthToken                     string
	Headers                                 http.Header
	AccountID, AuthRevision, ConversationID string
	Policy                                  WebSocketPolicy
	StillCurrent                            func() bool
}

type websocketObjectAcceptanceKey struct{}

type websocketObjectAcceptance struct {
	mu       sync.Mutex
	resolveF func(bool)
	resolved bool
}

func (a *websocketObjectAcceptance) hold(resolve func(bool)) {
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		resolve(false)
		return
	}
	previous := a.resolveF
	a.resolveF = resolve
	a.mu.Unlock()
	if previous != nil {
		previous(false)
	}
}

func (a *websocketObjectAcceptance) resolve(accepted bool) {
	a.mu.Lock()
	if a.resolved {
		a.mu.Unlock()
		return
	}
	a.resolved = true
	resolve := a.resolveF
	a.resolveF = nil
	a.mu.Unlock()
	if resolve != nil {
		resolve(accepted)
	}
}

// Config's OAuth tests consume this provider, so transport dependencies are injected.
type WebSocketOptions struct {
	Manager     WebSocketManager
	Current     func() (WebSocketPolicy, error)
	EncodeFrame func([]byte) ([]byte, error)
	HTTPClient  func() (*http.Client, error)
	// Endpoint is intentionally private: production always uses the Codex origin.
	endpoint string
}

func (m *languageModel) streamWebSocket(ctx context.Context, payload []byte, userAgent string, yield func(fantasy.StreamPart) bool) bool {
	options := m.provider.websocket
	if options == nil || ctx.Value(cliprovider.TitleGenerationContextKey) == true {
		return false
	}
	if options.Manager.Closed() {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("openai-codex websocket owner closed")})
		return true
	}
	conversation, _ := ctx.Value(cliprovider.SessionIDContextKey).(string)
	policy, err := options.Current()
	if err != nil {
		options.Manager.InvalidateConversation(conversation)
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return true
	}
	if policy.Transport == WebSocketTransportHTTP {
		options.Manager.InvalidateConversation(conversation)
		return false
	}
	if conversation == "" {
		return false
	}
	frame, err := options.EncodeFrame(payload)
	if err != nil {
		return false
	}
	used, ok := m.provider.source.Current()
	if !ok || strings.TrimSpace(used.AccessToken) == "" {
		options.Manager.InvalidateConversation(conversation)
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("openai-codex OAuth credentials unavailable")})
		return true
	}
	acceptance, _ := ctx.Value(websocketObjectAcceptanceKey{}).(*websocketObjectAcceptance)
	lease := WebSocketLease(nil)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			policy, err = options.Current()
			if err != nil {
				options.Manager.InvalidateConversation(conversation)
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
				return true
			}
			if policy.Transport == WebSocketTransportHTTP {
				return false
			}
		}
		lease, err = options.Manager.Acquire(ctx, m.webSocketConnectionOptions(options, policy, used, conversation, userAgent))
		if err == nil {
			break
		}
		if !options.Manager.UnauthorizedHandshake(err) {
			return false
		}
		if attempt != 0 || m.provider.source.Refresh(ctx, used) != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("openai-codex websocket authentication rejected")})
			return true
		}
		current, exists := m.provider.source.Current()
		if !exists || strings.TrimSpace(current.AccessToken) == "" || reflect.DeepEqual(current, used) {
			options.Manager.InvalidateConversation(conversation)
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("openai-codex websocket credentials unavailable after refresh")})
			return true
		}
		used = current
	}
	if lease == nil {
		return false
	}
	held := false
	defer func() {
		if !held {
			lease.Release()
		}
	}()
	conn := lease.Conn()
	current, exists := m.provider.source.Current()
	latestPolicy, policyErr := options.Current()
	if !exists || !reflect.DeepEqual(current, used) || policyErr != nil || !reflect.DeepEqual(policy, latestPolicy) {
		_ = conn.Close()
		return false
	}
	if err = conn.BeginExchange(); err != nil {
		_ = conn.Close()
		return false
	}
	if err = conn.SendRaw(ctx, frame); err != nil {
		var send WebSocketExchangeError
		if errors.As(err, &send) && send.RetrySafe() {
			return false
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return true
	}
	state := newStreamState()
	allAccepted, finishAccepted := true, false
	acceptPart := func(part fantasy.StreamPart) bool {
		accepted := yield(part)
		if !accepted {
			allAccepted = false
		}
		if part.Type == fantasy.StreamPartTypeFinish && accepted {
			finishAccepted = true
		}
		return accepted
	}
	for {
		raw, receiveErr := conn.ReceiveRaw(ctx)
		if receiveErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				receiveErr = conn.Abort(ctxErr)
			}
			acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: receiveErr})
			return true
		}
		if err = state.consume(raw, acceptPart); err != nil {
			if errors.Is(err, errStreamStopped) {
				_ = conn.Abort(err)
				return true
			}
			acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: webSocketStreamFailure(conn, err, state.acceptedOutput)})
			return true
		}
		if state.completed {
			// Incomplete responses cannot establish a reusable parent connection.
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Status string `json:"status"`
				} `json:"response"`
			}
			_ = json.Unmarshal(raw, &event)
			if event.Type == "response.completed" && event.Response.Status != "completed" {
				acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: conn.Abort(errors.New("invalid Codex terminal response"))})
				return true
			}
			if event.Type == "response.incomplete" && event.Response.Status != "incomplete" {
				acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: conn.Abort(errors.New("invalid Codex terminal response"))})
				return true
			}
			if event.Type != "response.completed" && event.Type != "response.incomplete" {
				acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: conn.Abort(errors.New("invalid Codex terminal response"))})
				return true
			}
			state.finish(acceptPart)
			if event.Type == "response.incomplete" && event.Response.Status == "incomplete" {
				_ = conn.Close()
				return true
			}
			if !allAccepted || !finishAccepted || ctx.Err() != nil {
				_ = conn.Close()
				return true
			}
			if event.Type != "response.completed" || event.Response.Status != "completed" {
				_ = conn.Close()
				return true
			}
			if acceptance != nil {
				acceptance.hold(func(accepted bool) {
					if accepted && ctx.Err() == nil {
						if err := conn.MarkCompleted(); err == nil {
							lease.Release()
							return
						}
					}
					_ = conn.Close()
					lease.Release()
				})
				held = true
			} else if err = conn.MarkCompleted(); err != nil {
				acceptPart(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: conn.Abort(err)})
			}
			return true
		}
	}
}

func (m *languageModel) webSocketConnectionOptions(options *WebSocketOptions, policy WebSocketPolicy, token oauth.Token, conversation, userAgent string) WebSocketRequest {
	endpoint := options.endpoint
	if endpoint == "" {
		endpoint = "wss://chatgpt.com/backend-api/codex/responses"
	}
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "rush"
	}
	revision := sha256.Sum256([]byte(token.AccessToken))
	headers := http.Header{}
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	headers.Set("originator", "omp")
	headers.Set("version", codexVersion)
	headers.Set("User-Agent", userAgent)
	headers.Set("x-codex-routing-hint", "model="+m.modelID)
	headers.Set("conversation_id", conversation)
	headers.Set("session_id", conversation)
	headers.Set("x-client-request-id", conversation)
	if token.AccountID != "" {
		headers.Set("chatgpt-account-id", token.AccountID)
	}
	account := token.AccountID
	if account == "" {
		account = hex.EncodeToString(revision[:])
	}
	return WebSocketRequest{
		Endpoint: endpoint, AuthToken: token.AccessToken, Headers: headers, Policy: policy,
		StillCurrent: func() bool {
			current, ok := m.provider.source.Current()
			latest, err := options.Current()
			return ok && err == nil && reflect.DeepEqual(current, token) && latest.Transport == policy.Transport && latest.NetworkIdentity == policy.NetworkIdentity
		},
		AccountID: account, AuthRevision: hex.EncodeToString(revision[:]) + ":" + strconv.FormatUint(policy.Revision, 10), ConversationID: conversation,
	}
}
