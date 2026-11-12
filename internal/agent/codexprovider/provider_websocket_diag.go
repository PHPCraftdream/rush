package codexprovider

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/session"
)

// Transport names and HTTP-fallback reasons reported by the transport record.
const (
	transportWebSocket = "websocket"
	transportHTTP      = "http"

	fallbackPolicyHTTP       = "policy_http"
	fallbackNoConversation   = "no_conversation"
	fallbackFrameEncode      = "frame_encode"
	fallbackAcquire          = "acquire_failed"
	fallbackStaleCredentials = "credentials_or_policy_changed"
	fallbackBeginExchange    = "exchange_state"
	fallbackSendNotSent      = "send_not_sent"
	fallbackChainRecovery    = "chain_recovery_unavailable"
)

// transportObservation collects one model call's transport facts so a
// WebSocket turn and an HTTP turn emit comparable debug records.
type transportObservation struct {
	started   time.Time
	model     string
	transport string
	fallback  string
	frame     []any
	recovered int
	outcome   string
	usage     fantasy.Usage
	hasUsage  bool
}

func newTransportObservation(model string) *transportObservation {
	return &transportObservation{started: time.Now(), model: model, transport: transportHTTP, outcome: "incomplete"}
}

// wrap records the terminal part of the stream without altering it.
func (o *transportObservation) wrap(yield func(fantasy.StreamPart) bool) func(fantasy.StreamPart) bool {
	return func(part fantasy.StreamPart) bool {
		switch part.Type {
		case fantasy.StreamPartTypeFinish:
			o.outcome, o.usage, o.hasUsage = "finished", part.Usage, true
		case fantasy.StreamPartTypeError:
			o.outcome = "error"
		}
		return yield(part)
	}
}

func (o *transportObservation) useHTTP(reason string) {
	if o == nil {
		return
	}
	o.transport, o.fallback, o.frame = transportHTTP, reason, nil
}

func (o *transportObservation) useWebSocket(conn WebSocketConnection) {
	if o == nil {
		return
	}
	o.transport, o.fallback = transportWebSocket, ""
	if source, ok := conn.(interface{ FrameAttrs() []any }); ok {
		o.frame = source.FrameAttrs()
	}
}

// log emits the record; usage counters are the provider-reported ones, so a
// zero cached_tokens is "reported zero" only when has_usage is true.
func (o *transportObservation) log(ctx context.Context) {
	if o == nil || !slog.Default().Enabled(ctx, slog.LevelDebug) {
		return
	}
	conversation, _ := ctx.Value(cliprovider.SessionIDContextKey).(string)
	attrs := []any{
		"provider", Name, "model", o.model,
		"session_hash", session.HashID(conversation),
		"transport", o.transport,
		"http_fallback_reason", o.fallback,
		"chain_recoveries", o.recovered,
		"outcome", o.outcome,
		"has_usage", o.hasUsage,
		"input_tokens", o.usage.InputTokens,
		"cache_read_tokens", o.usage.CacheReadTokens,
		"elapsed_ms", time.Since(o.started).Milliseconds(),
	}
	slog.Debug("responses transport exchange", append(attrs, o.frame...)...)
}

// codexChainRejected reports a server rejection of an unknown
// previous_response_id that carries no output, so the request was not processed.
func codexChainRejected(data []byte) bool {
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	kind := rawString(event["type"])
	if (kind != "error" && kind != "response.failed") || codexEventHasOutput(event) {
		return false
	}
	var failure struct {
		Code     string       `json:"code"`
		Error    codexFailure `json:"error"`
		Response struct {
			Error codexFailure `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &failure) != nil {
		return false
	}
	const code = "previous_response_not_found"
	if kind == "response.failed" {
		return failure.Response.Error.Code == code
	}
	return failure.Error.Code == code || failure.Code == code
}

func webSocketChained(conn WebSocketConnection) bool {
	chained, ok := conn.(interface{ Chained() bool })
	return ok && chained.Chained()
}
