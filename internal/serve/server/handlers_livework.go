package server

// Batch-derived live-work snapshot emitter for the existing flat web panel.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// titleMaxLen bounds the abbreviated command/prompt shown in the panel
// (in RUNES — byte slicing would corrupt multi-byte UTF-8).
const titleMaxLen = 120

// liveWorkTerminalLimit is how many finished rows per list (Commands /
// Agents) a snapshot carries: every running row plus the most recently
// updated finished ones. Without it a long-lived session's panel grows
// without bound.
const liveWorkTerminalLimit = 50

const (
	// liveWorkCoalesceDelay is the dirty-window: marks arriving within it
	// collapse into ONE snapshot per session.
	liveWorkCoalesceDelay = 250 * time.Millisecond
	// liveWorkMarkBuffer bounds the mark channel; a saturated channel means
	// a sweep is already overdue, and dropping a mark is always safe (the
	// next event re-marks).
	liveWorkMarkBuffer = 256
)

// handleGetSessionLiveWork replies to CmdGetSessionLiveWork with one full
// EventSessionLiveWork snapshot for the requested session and its delegation
// descendants. A session that does not exist is an explicit error — the
// snapshot must never be synthesized from other sessions' rows (isolation:
// only the requested session's tree is ever walked, and a job row never
// leaves its owner's tree).
func handleGetSessionLiveWork(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p GetSessionLiveWorkPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.Reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.Reply(msg.ID, EventError, nil, "session not found")
		return
	}
	snap, err := buildSessionLiveWork(ctx, a, c.Hub().LiveWorkTitles(), p.SessionID)
	if err != nil {
		c.Reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.Reply(msg.ID, EventSessionLiveWork, snap, "")
}

// buildSessionLiveWork reads the flat projection from batch_nodes and
// session_notices. A partial read is marked incomplete and is never used to
// replace a previously shown snapshot.
func buildSessionLiveWork(ctx context.Context, a *appPkg.App, cache *titleCache, sessionID string) (SessionLiveWorkPayload, error) {
	return buildSessionLiveWorkFromBatch(ctx, a, cache, sessionID)
}

// liveWorkTitleJob is the batch-root identity a title lookup is for: the
// owner session, the root's tool call id and input hash (the same three
// facts the old async_jobs row carried — the batch root row carries them
// as tool_call_id / input_hash).
type liveWorkTitleJob struct {
	owner      string
	toolCallID string
	inputHash  string
}

// titles fills the requested rows' titles from the cache and, ONLY for
// keys the cache is missing, reads the owner sessions' history (the
// command/prompt text a job was started with lives only in the assistant
// message's tool_call part — the batch root row stores just its hash). Missing
// text (no message, or an input shape with nothing readable) falls back to
// the row's result_summary at the caller.
//
// Sprint 0 L4 (S4): was the (c *titleCache) titles method; the cache moved to
// hub.TitleCache (handlers_livework.go:135-179 at HEAD) and this stays as a
// free function because it reads internal/app history (§2.6 forbids app in
// hub). Body otherwise verbatim; c.get/c.put became cache.Get/cache.Put and
// the key construction is explicit because liveWorkTitleJob no longer
// converts to the (renamed, field-renamed) hub.TitleKey by identity.
func titles(ctx context.Context, a *appPkg.App, cache *hub.TitleCache, jobs []liveWorkTitleJob) map[hub.TitleKey]string {
	out := make(map[hub.TitleKey]string, len(jobs))
	missing := make(map[hub.TitleKey]struct{})
	for _, j := range jobs {
		key := hub.TitleKey{Owner: j.owner, ToolCallID: j.toolCallID, InputHash: j.inputHash}
		if t, ok := cache.Get(key); ok {
			out[key] = t
		} else {
			missing[key] = struct{}{}
		}
	}
	if len(missing) == 0 {
		return out
	}

	owners := make(map[string]struct{})
	for key := range missing {
		owners[key.Owner] = struct{}{}
	}
	for sessionID := range owners {
		msgs, _, err := a.Messages.ListWithWatermark(ctx, sessionID)
		if err != nil {
			slog.Warn("live_work: could not read history for job titles", "session", sessionID, "err", err)
			continue
		}
		for _, m := range msgs {
			for _, part := range m.Parts {
				tc, ok := part.(message.ToolCall)
				if !ok || tc.Input == "" {
					continue
				}
				for key := range missing {
					if key.ToolCallID != tc.ID || key.Owner != m.SessionID {
						continue
					}
					title := abbreviateToolInput(tc.Input)
					cache.Put(key, title)
					out[key] = title
					delete(missing, key)
				}
			}
		}
	}
	return out
}

// abbreviateToolInput extracts the human-readable payload of a tool call's
// input JSON ("command" for bash/run_command, "prompt"/"url" otherwise) and
// collapses it to one bounded line.
func abbreviateToolInput(input string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return oneLine(input)
	}
	for _, key := range []string{"command", "prompt", "url"} {
		if v, ok := fields[key].(string); ok && v != "" {
			return truncateTitle(oneLine(v))
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateTitle cuts by RUNES, never bytes: s[:n] would split a multi-byte
// UTF-8 sequence (Cyrillic, emoji) into invalid bytes.
func truncateTitle(s string) string {
	if utf8.RuneCountInString(s) <= titleMaxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:titleMaxLen]) + "…"
}

// shouldPushLiveWork reports whether a message event signals async-work
// state change: a notice (job completion, session notice) or a message
// carrying tool parts (the "started" ack, the tool_call itself). Plain
// prose never earns a snapshot; there is no polling loop.
func shouldPushLiveWork(m message.Message) bool {
	if m.NoticeKind != "" {
		return true
	}
	for _, part := range m.Parts {
		switch part.(type) {
		case message.ToolCall, message.ToolResult:
			return true
		}
	}
	return false
}

// liveWorkPusher coalesces per-session dirty marks and builds the snapshots
// OFF the broadcast hot path: the event forwarder only calls mark, which
// never blocks; one coalescing window per burst produces one snapshot per
// dirty session.
type liveWorkPusher struct {
	ctx     context.Context
	a       *appPkg.App
	h       *Hub
	marks   chan string
	stopped chan struct{}
}

func startLiveWorkPusher(ctx context.Context, a *appPkg.App, h *Hub) *liveWorkPusher {
	p := &liveWorkPusher{
		ctx:     ctx,
		a:       a,
		h:       h,
		marks:   make(chan string, liveWorkMarkBuffer),
		stopped: make(chan struct{}),
	}
	go p.run()
	return p
}

// mark records a dirty session. Non-blocking by contract: a full channel
// means a sweep is overdue anyway and the next event re-marks the session.
func (p *liveWorkPusher) mark(sessionID string) {
	select {
	case p.marks <- sessionID:
	default:
	}
}

// run drains marks into the dirty set and sweeps once per coalescing
// window. Exits when the server ctx is done; stopped signals that.
func (p *liveWorkPusher) run() {
	defer close(p.stopped)
	dirty := make(map[string]struct{})
	var timer <-chan time.Time
	for {
		select {
		case <-p.ctx.Done():
			return
		case id := <-p.marks:
			dirty[id] = struct{}{}
			if timer == nil {
				timer = time.After(liveWorkCoalesceDelay)
			}
		case <-timer:
			timer = nil
			for id := range dirty {
				p.pushOne(id)
			}
			dirty = make(map[string]struct{})
		}
	}
}

func (p *liveWorkPusher) pushOne(sessionID string) {
	// A session deleted between the mark and the sweep gets NO snapshot:
	// its rows are gone (session delete cascades) and an empty snapshot
	// would only be noise.
	if _, err := p.a.Sessions.Get(p.ctx, sessionID); err != nil {
		return
	}
	snap, err := buildSessionLiveWork(p.ctx, p.a, p.h.LiveWorkTitles(), sessionID)
	if err != nil {
		slog.Warn("live_work: could not build the change snapshot", "session", sessionID, "err", err)
		return
	}
	// R12-C: an incomplete ON-path read is not broadcast — the client KEEPS
	// the work it already shows instead of replacing it with a partial
	// snapshot (a partial read never silently drops active work). Complete
	// is a pointer: nil means the flag-OFF path built the snapshot, which is
	// always broadcast unchanged.
	if snap.Complete != nil && !*snap.Complete {
		slog.Warn("live_work: skipping an incomplete batch-read snapshot", "session", sessionID)
	} else {
		p.h.Broadcast(EventSessionLiveWork, snap)
	}
	// The flat batch projection also carries structured snapshots in the same coalescing window.
	if bsnap := buildBatchWorkSnapshot(p.ctx, p.a, sessionID); bsnap.SessionID != "" {
		p.h.Broadcast(EventSessionBatchWork, bsnap)
	}
	// Stage 5b: the same coalescing window also carries the wake-schedule
	// snapshot — a schedule's create/fire/cancel always arrives as a notice
	// or tool message, so the existing marks cover it with no extra
	// subscription and no DB polling.
	pushWakeSchedules(p.ctx, p.a, p.h, sessionID)
}
