package nettransport

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// Frame modes reported by ResponsesWSFrameInfo.
const (
	ResponsesWSFrameFull  = "full"
	ResponsesWSFrameDelta = "delta"
)

// Reasons a frame stayed full. They describe the local decision only.
const (
	ResponsesWSFullDisabled        = "delta_disabled"
	ResponsesWSFullTooLarge        = "frame_too_large"
	ResponsesWSFullUnparseable     = "unparseable"
	ResponsesWSFullIneligible      = "ineligible_request"
	ResponsesWSFullFirstExchange   = "first_exchange"
	ResponsesWSFullNoParent        = "no_parent"
	ResponsesWSFullOptionsChanged  = "options_changed"
	ResponsesWSFullOverBudget      = "over_budget"
	ResponsesWSFullHistoryDiverged = "history_diverged"
	ResponsesWSFullNotSmaller      = "not_smaller"
)

// ResponsesWSFrameInfo records why one exchange used a full or delta frame.
// Sizes are bytes on the wire; InputItems/WireItems are -1 when the request
// was not parsed. DivergedAt is the index of the first history item that broke
// the chain, or -1.
type ResponsesWSFrameInfo struct {
	Mode       string
	Reason     string
	FullBytes  int
	WireBytes  int
	InputItems int
	WireItems  int
	DivergedAt int
}

func newResponsesWSFrameInfo(frameBytes int) ResponsesWSFrameInfo {
	return ResponsesWSFrameInfo{Mode: ResponsesWSFrameFull, FullBytes: frameBytes, WireBytes: frameBytes, InputItems: -1, WireItems: -1, DivergedAt: -1}
}

// ResponsesWSChainRejected reports whether raw is a server error event saying
// the previous_response_id of a chained frame is unknown. Such a rejection is
// issued before any generation, so a full frame may be sent on a fresh connection.
func ResponsesWSChainRejected(raw []byte) bool {
	var event struct {
		Type     string                `json:"type"`
		Code     string                `json:"code"`
		Error    struct{ Code string } `json:"error"`
		Response struct {
			Error struct{ Code string } `json:"error"`
		} `json:"response"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &event) != nil {
		return false
	}
	const code = "previous_response_not_found"
	switch event.Type {
	case "error":
		return event.Error.Code == code || event.Code == code
	case "response.failed":
		return event.Response.Error.Code == code
	}
	return false
}

// Chained reports whether the current or last frame sent was a delta.
func (c *ResponsesWSConn) Chained() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.frameInfo.Mode == ResponsesWSFrameDelta
}

// FrameInfo returns the frame decision of the current or last exchange.
func (c *ResponsesWSConn) FrameInfo() ResponsesWSFrameInfo {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.frameInfo
}

// FrameAttrs returns structured log fields describing this connection's
// current exchange: the frame decision and cumulative per-connection counters.
func (c *ResponsesWSConn) FrameAttrs() []any {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.frameAttrsLocked()
}

func (c *ResponsesWSConn) frameAttrsLocked() []any {
	info := c.frameInfo
	return []any{
		"exchange_seq", c.exchanges,
		"conn_age_ms", time.Since(c.created).Milliseconds(),
		"frame_mode", info.Mode,
		"frame_reason", info.Reason,
		"frame_full_bytes", info.FullBytes,
		"frame_wire_bytes", info.WireBytes,
		"input_items", info.InputItems,
		"wire_items", info.WireItems,
		"diverged_at", info.DivergedAt,
		"conn_delta_exchanges", c.deltaExchanges,
		"conn_full_bytes_total", c.fullBytesTotal,
		"conn_wire_bytes_total", c.wireBytesTotal,
	}
}

// diagAttrsLocked adds the connection identity to the frame fields. The
// session is reported as a hash only; callers hold stateMu.
func (c *ResponsesWSConn) diagAttrsLocked() []any {
	return append([]any{"protocol", c.protocol, "session_hash", session.HashID(c.owner.ConversationID)}, c.frameAttrsLocked()...)
}

func debugEnabled() bool {
	return slog.Default().Enabled(context.Background(), slog.LevelDebug)
}
