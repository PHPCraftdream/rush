package server

// Live-work snapshot emitter (task #1058): the read-only server half of the
// web panel's Commands/Agents tabs. Data comes from the durable async_jobs
// table through session.AsyncJobStore's readers (JobsInTree, the read-pool
// reader), never from this process's memory — work hosted by another process
// (a live `rush run`) reads exactly the same way. Schedules are NOT part of
// this snapshot (a separate stage).
//
// Push path: the message-event forwarder only MARKS a session dirty; a
// dedicated worker coalesces marks per session (one ~250ms window = one
// snapshot, however many tool messages arrived) and does the DB reads off
// the broadcast hot path. The worker exits with the server ctx.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
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
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	if _, err := a.Sessions.Get(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	snap, err := buildSessionLiveWork(ctx, a, c.hub.liveWorkTitles, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventSessionLiveWork, snap, "")
}

// buildSessionLiveWork reads every async_jobs row in the session's
// delegation tree (ANY state — the panel distinguishes running from finished
// with its cause) and splits them into commands and sub-agent delegations.
// A 'running' row whose host is provably dead is reported AS RUNNING until
// dead-host recovery transitions it (DUR-6): this read-only snapshot never
// probes hosts itself, so a stale 'running' can be visible for up to the
// recovery sweep interval — by design, not a defect.
func buildSessionLiveWork(ctx context.Context, a *appPkg.App, cache *titleCache, sessionID string) (SessionLiveWorkPayload, error) {
	snap := SessionLiveWorkPayload{SessionID: sessionID, Commands: []LiveWorkItemWire{}, Agents: []LiveWorkItemWire{}}
	store := a.AsyncJobStore()
	if store == nil {
		return snap, nil
	}
	jobs, incomplete := store.JobsInTree(ctx, sessionID)
	if incomplete {
		// Same rule as every other reader of this walk (ASYNC-02): a short
		// result may only mean "possibly more", never "nothing". A partial
		// list is still a valid snapshot; the next change event re-sends it.
		slog.Warn("live_work: the delegation-tree walk was incomplete")
	}
	if len(jobs) == 0 {
		return snap, nil
	}

	// Awaiting-answer badges (#1158): the owner's pending child_question
	// notices are matched to their held delegation rows by the notice's
	// job_tool_call_id. No new polling: the snapshot is built on the same
	// marks and on-demand requests as before.
	questions := map[string]session.ChildQuestion{}
	if qs, err := store.PendingChildQuestions(ctx, sessionID); err == nil {
		for _, q := range qs {
			if q.DelegationToolCallID != "" {
				questions[q.DelegationToolCallID] = q
			}
		}
	}

	commands := capLiveWorkRows(jobs, string(session.JobKindCommand))
	agents := capLiveWorkRows(jobs, string(session.JobKindAgent))
	fetches := capLiveWorkRows(jobs, string(session.JobKindFetch))
	agents = append(agents, fetches...)
	titles := cache.titles(ctx, a, append(append([]db.AsyncJob{}, commands...), agents...))

	fill := func(rows []db.AsyncJob) []LiveWorkItemWire {
		items := make([]LiveWorkItemWire, 0, len(rows))
		for _, row := range rows {
			title := titles[liveWorkTitleKey{row.OwnerSessionID, row.ToolCallID, row.InputHash}]
			item := LiveWorkItemWire{
				ToolCallID:     row.ToolCallID,
				ToolName:       row.ToolName,
				Title:          title,
				ChildSessionID: row.ChildSessionID.String,
				StartedAt:      row.CreatedAt * 1000,
				Status:         row.State,
				Reason:         row.NoticeKind,
				LastActivityAt: row.UpdatedAt * 1000,
			}
			if item.Title == "" {
				item.Title = row.ResultSummary.String
			}
			if q, ok := questions[row.ToolCallID]; ok {
				item.AwaitingAnswer = true
				item.AwaitingQuestion = q.Question
			}
			if row.State != "running" {
				item.FinishedAt = row.UpdatedAt * 1000
			}
			items = append(items, item)
		}
		return items
	}
	snap.Commands = fill(commands)
	snap.Agents = fill(agents)
	return snap, nil
}

// capLiveWorkRows selects one kind's rows: every 'running' row plus the
// liveWorkTerminalLimit most recently updated finished rows, the whole
// result sorted newest-first (deterministic tie-break on tool_call_id, so
// rows updated in the same second keep a stable order).
func capLiveWorkRows(rows []db.AsyncJob, kind string) []db.AsyncJob {
	sameKind := make([]db.AsyncJob, 0, len(rows))
	for _, row := range rows {
		if row.Kind == kind {
			sameKind = append(sameKind, row)
		}
	}
	sort.Slice(sameKind, func(i, j int) bool {
		if sameKind[i].UpdatedAt != sameKind[j].UpdatedAt {
			return sameKind[i].UpdatedAt > sameKind[j].UpdatedAt
		}
		return sameKind[i].ToolCallID < sameKind[j].ToolCallID
	})
	kept := make([]db.AsyncJob, 0, len(sameKind))
	terminal := 0
	for _, row := range sameKind {
		if row.State != "running" {
			terminal++
			if terminal > liveWorkTerminalLimit {
				continue
			}
		}
		kept = append(kept, row)
	}
	return kept
}

// liveWorkTitleKey identifies ONE async_jobs row's title. tool_call_id is
// NOT unique (providers number calls per response, and a reused id is
// archived and re-claimed), so the key carries the owner session AND the
// row's input_hash: input_hash changes exactly when a row is re-claimed
// with different input, which is the only way the title text can change —
// same hash means the same input, so the cached title is still correct.
// Per-hub (see Hub.liveWorkTitles), never package-global: a global
// id-keyed cache leaked one session's titles into another session's panel.
type liveWorkTitleKey struct {
	owner      string
	toolCallID string
	inputHash  string
}

// titleCache is a bounded per-hub tool-call title cache so a repeated
// snapshot does not re-read session history: only keys MISSING from the
// cache trigger a history read at all. FIFO eviction at titleCacheCap.
type titleCache struct {
	mu      sync.Mutex
	entries map[liveWorkTitleKey]string
	fifo    []liveWorkTitleKey
}

const titleCacheCap = 4096

func newTitleCache() *titleCache {
	return &titleCache{entries: make(map[liveWorkTitleKey]string)}
}

func (c *titleCache) get(key liveWorkTitleKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.entries[key]
	return t, ok
}

func (c *titleCache) put(key liveWorkTitleKey, title string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	c.entries[key] = title
	c.fifo = append(c.fifo, key)
	for len(c.fifo) > titleCacheCap {
		delete(c.entries, c.fifo[0])
		c.fifo = c.fifo[1:]
	}
}

// titles fills the requested rows' titles from the cache and, ONLY for
// keys the cache is missing, reads the owner sessions' history (the
// command/prompt text a job was started with lives only in the assistant
// message's tool_call part — async_jobs stores just its hash). Missing
// text (no message, or an input shape with nothing readable) falls back to
// the row's result_summary at the caller.
func (c *titleCache) titles(ctx context.Context, a *appPkg.App, jobs []db.AsyncJob) map[liveWorkTitleKey]string {
	out := make(map[liveWorkTitleKey]string, len(jobs))
	missing := make(map[liveWorkTitleKey]struct{})
	for _, j := range jobs {
		key := liveWorkTitleKey{j.OwnerSessionID, j.ToolCallID, j.InputHash}
		if t, ok := c.get(key); ok {
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
		owners[key.owner] = struct{}{}
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
					if key.toolCallID != tc.ID || key.owner != m.SessionID {
						continue
					}
					title := abbreviateToolInput(tc.Input)
					c.put(key, title)
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
	snap, err := buildSessionLiveWork(p.ctx, p.a, p.h.liveWorkTitles, sessionID)
	if err != nil {
		slog.Warn("live_work: could not build the change snapshot", "session", sessionID, "err", err)
		return
	}
	p.h.Broadcast(EventSessionLiveWork, snap)
	// Stage 5b: the same coalescing window also carries the wake-schedule
	// snapshot — a schedule's create/fire/cancel always arrives as a notice
	// or tool message, so the existing marks cover it with no extra
	// subscription and no DB polling.
	pushWakeSchedules(p.ctx, p.a, p.h, sessionID)
}
