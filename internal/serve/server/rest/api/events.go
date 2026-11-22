package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire/finalgate"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

const (
	apiEventMaxPerSession = 1000
	APIEventMaxSessions   = 200
	APIEventSubscriberCap = 256
)

type apiEventSession struct {
	events           []APIEvent
	subscribers      map[*apiEventSubscriber]struct{}
	lastUse          uint64
	truncatedThrough uint64
}

type apiEventSubscriber struct {
	ch       chan APIEvent
	overflow chan struct{}
	dropped  bool
}

// APIEventLog retains a bounded, route-set-local replay window per session.
type APIEventLog struct {
	mu            sync.Mutex
	sessions      map[string]*apiEventSession
	now           func() time.Time
	MaxPerSession int
	nextID        uint64
	useSeq        uint64
}

func NewAPIEventLog(now func() time.Time) *APIEventLog {
	if now == nil {
		now = time.Now
	}
	return &APIEventLog{sessions: make(map[string]*apiEventSession), now: now, MaxPerSession: apiEventMaxPerSession}
}

func (l *APIEventLog) Publish(sessionID, eventType string, data any) (APIEvent, bool) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return APIEvent{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessionLocked(sessionID)
	if s == nil {
		return APIEvent{}, false
	}
	l.nextID++
	event := APIEvent{ID: l.nextID, Type: eventType, Data: encoded, At: l.now().UTC()}
	s.events = append(s.events, event)
	if len(s.events) > l.MaxPerSession {
		s.truncatedThrough = s.events[0].ID
		s.events = s.events[1:]
	}
	for subscriber := range s.subscribers {
		if subscriber.dropped {
			continue
		}
		select {
		case subscriber.ch <- event:
		default:
			subscriber.dropped = true
			delete(s.subscribers, subscriber)
			close(subscriber.overflow)
		}
	}
	return event, true
}

func (l *APIEventLog) Replay(sessionID string, after uint64) (events []APIEvent, oldest uint64, retained bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessions[sessionID]
	if s == nil {
		return nil, 0, false
	}
	l.touchLocked(s)
	if len(s.events) == 0 {
		return nil, s.truncatedThrough + 1, true
	}
	oldest = s.events[0].ID
	if s.truncatedThrough > 0 {
		oldest = s.truncatedThrough + 1
	}
	for _, event := range s.events {
		if event.ID > after {
			events = append(events, cloneAPIEvent(event))
		}
	}
	return events, oldest, true
}

func (l *APIEventLog) Subscribe(sessionID string) (<-chan APIEvent, <-chan struct{}, func()) {
	l.mu.Lock()
	s := l.sessionLocked(sessionID)
	if s == nil {
		l.mu.Unlock()
		return nil, nil, func() {}
	}
	subscriber := &apiEventSubscriber{ch: make(chan APIEvent, APIEventSubscriberCap), overflow: make(chan struct{})}
	s.subscribers[subscriber] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			l.mu.Lock()
			delete(s.subscribers, subscriber)
			l.mu.Unlock()
		})
	}
	return subscriber.ch, subscriber.overflow, unsubscribe
}

func (l *APIEventLog) DeleteSession(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.sessions[sessionID]; s != nil {
		for subscriber := range s.subscribers {
			delete(s.subscribers, subscriber)
			close(subscriber.ch)
		}
		delete(l.sessions, sessionID)
	}
}

func (l *APIEventLog) SessionCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.sessions)
}

func (l *APIEventLog) SubscriberCount(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.sessions[sessionID]; s != nil {
		return len(s.subscribers)
	}
	return 0
}

func (l *APIEventLog) sessionLocked(id string) *apiEventSession {
	if s := l.sessions[id]; s != nil {
		l.touchLocked(s)
		return s
	}
	if len(l.sessions) >= APIEventMaxSessions {
		oldestID := ""
		var oldest *apiEventSession
		for key, candidate := range l.sessions {
			if len(candidate.subscribers) != 0 {
				continue
			}
			if oldest == nil || candidate.lastUse < oldest.lastUse {
				oldestID, oldest = key, candidate
			}
		}
		if oldest != nil {
			delete(l.sessions, oldestID)
		}
	}
	// Soft cap: when every retained session has a live stream nothing may be
	// evicted, and dropping the event (or handing a stream a nil channel) would
	// silently break delivery. The overshoot is bounded by the open streams.
	s := &apiEventSession{subscribers: make(map[*apiEventSubscriber]struct{})}
	l.sessions[id] = s
	l.touchLocked(s)
	return s
}

func (l *APIEventLog) touchLocked(s *apiEventSession) {
	l.useSeq++
	s.lastUse = l.useSeq
}

func cloneAPIEvent(event APIEvent) APIEvent {
	event.Data = append(json.RawMessage(nil), event.Data...)
	return event
}

type APIEventFeeder struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func StartAPIEventFeeder(app *appPkg.App, log *APIEventLog, registry *APIRunRegistry, gates ...session.FinalizationTerminalGate) *APIEventFeeder {
	ctx, cancel := context.WithCancel(context.Background())
	feeder := &APIEventFeeder{cancel: cancel, done: make(chan struct{})}
	if registry != nil {
		registry.SetObserver(func(run APIRun) {
			data := struct {
				RunID     string      `json:"run_id"`
				State     APIRunState `json:"state"`
				ErrorCode string      `json:"error_code,omitempty"`
			}{RunID: run.ID, State: run.State}
			if run.Error != nil {
				data.ErrorCode = run.Error.Code
			}
			log.Publish(run.SessionID, "run", data)
		})
	}
	go func() {
		defer close(feeder.done)
		if app == nil {
			<-ctx.Done()
			return
		}
		// Fork patch (finalization): nil gate (flag off) leaves both feeds
		// byte-for-byte unchanged; a held terminal event waits for the seal.
		var gate session.FinalizationTerminalGate
		if len(gates) > 0 {
			gate = gates[0]
		} else {
			gate = finalgate.GateFor(app)
		}
		var wg sync.WaitGroup
		if app.Messages != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range finalgate.FinalizationGatedMessageStream(ctx, gate, app.Messages.Subscribe(ctx)) {
					if event.Type == pubsub.CreatedEvent || event.Type == pubsub.UpdatedEvent {
						log.Publish(event.Payload.SessionID, "message", wire.APIMessageFromModel(event.Payload))
					}
				}
			}()
		}
		if app.Sessions != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range finalgate.FinalizationGatedSessionStream(ctx, gate, app.Sessions.Subscribe(ctx)) {
					switch event.Type {
					case pubsub.CreatedEvent, pubsub.UpdatedEvent:
						log.Publish(event.Payload.ID, "session", wire.APISessionFromModel(event.Payload))
					case pubsub.DeletedEvent:
						log.DeleteSession(event.Payload.ID)
					}
				}
			}()
		}
		<-ctx.Done()
		wg.Wait()
	}()
	return feeder
}

func (f *APIEventFeeder) Stop(ctx context.Context) error {
	if f == nil {
		return nil
	}
	f.cancel()
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

const APISSEStreamsPerToken = 8

type APIEventsSessionStore interface {
	GetSession(*http.Request, string) (session.Session, error)
	OwnsSession(session.Session) bool
}

type APISSETicker interface {
	C() <-chan time.Time
	Stop()
}

type APISSETickerFactory func() APISSETicker

type APISSELimiter struct {
	mu     sync.Mutex
	Counts map[string]int
}

type apiEventsHandler struct {
	store   APIEventsSessionStore
	log     *APIEventLog
	limiter *APISSELimiter
	ticker  APISSETickerFactory
	gate    session.FinalizationTerminalGate
}

func NewAPIEventsHandler(store APIEventsSessionStore, log *APIEventLog, limiter *APISSELimiter, ticker APISSETickerFactory, gates ...session.FinalizationTerminalGate) http.HandlerFunc {
	if limiter == nil {
		limiter = &APISSELimiter{Counts: make(map[string]int)}
	}
	if ticker == nil {
		ticker = func() APISSETicker { return apiRealSSETicker{ticker: time.NewTicker(15 * time.Second)} }
	}
	h := apiEventsHandler{store: store, log: log, limiter: limiter, ticker: ticker}
	// Fork patch (finalization): an explicit gate (test seam) wins;
	// otherwise the production store derives it from its app, nil with the
	// flag off.
	if len(gates) > 0 {
		h.gate = gates[0]
	} else if ws, ok := store.(APIAppWriteStore); ok {
		h.gate = finalgate.GateFor(ws.App)
	}
	return h.serveHTTP
}

type apiRealSSETicker struct{ ticker *time.Ticker }

func (t apiRealSSETicker) C() <-chan time.Time { return t.ticker.C }
func (t apiRealSSETicker) Stop()               { t.ticker.Stop() }

func (h apiEventsHandler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	item, err := h.store.GetSession(r, r.PathValue("id"))
	if err != nil || !h.store.OwnsSession(item) {
		ApiNotFound(w, r, "session")
		return
	}
	after, present, err := apiSSELastID(r)
	if err != nil {
		WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid Last-Event-ID", "", nil)
		return
	}
	principal, _ := PrincipalFrom(r.Context())
	if !h.limiter.acquire(principal.TokenID) {
		w.Header().Set("Retry-After", "5")
		WriteAPIProblem(w, r, http.StatusTooManyRequests, "Too Many Requests", "too many event streams", "", nil)
		return
	}
	defer h.limiter.release(principal.TokenID)
	ch, overflow, unsubscribe := h.log.Subscribe(item.ID)
	defer unsubscribe()
	// Fork patch (finalization): nil gate (flag off) returns ch unchanged;
	// a held terminal "run" event waits for the owner's seal. NOTE: the
	// Last-Event-ID replay below bypasses this gate (known gap, out of
	// scope).
	ch = finalgate.FinalizationGatedAPIEventStream(r.Context(), h.gate, item.ID, ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	flush := func() error { return http.NewResponseController(w).Flush() }
	if err := flush(); err != nil {
		return
	}
	last := uint64(0)
	if present {
		events, oldest, retained := h.log.Replay(item.ID, after)
		if retained && oldest != 0 && after < oldest-1 {
			if err := apiSSEWrite(w, APIEvent{Type: "reset", Data: json.RawMessage(`{"reason":"history_truncated"}`)}); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		}
		last = after
		for _, event := range events {
			if event.ID <= last {
				continue
			}
			if err := apiSSEWrite(w, event); err != nil {
				return
			}
			last = event.ID
		}
		if err := flush(); err != nil {
			return
		}
	}
	replayed := last
	ticker := h.ticker()
	defer ticker.Stop()
	revoked := APISSERevocation(r.Context(), principal)
	for {
		select {
		case <-revoked:
			_ = apiSSEWrite(w, APIEvent{Type: "closed", Data: json.RawMessage(`{"reason":"token_revoked"}`)})
			_ = flush()
			return
		case <-r.Context().Done():
			return
		case <-overflow:
			_ = apiSSEWrite(w, APIEvent{Type: "overflow", Data: json.RawMessage(`{}`)})
			_ = flush()
			return
		case event, ok := <-ch:
			if !ok {
				_ = apiSSEWrite(w, APIEvent{Type: "closed", Data: json.RawMessage(`{"reason":"session_deleted"}`)})
				_ = flush()
				return
			}
			// Live events dedupe only against the replay boundary: a held
			// terminal event legitimately arrives after higher event ids,
			// so a running high-water mark would drop it forever.
			if event.ID <= replayed {
				continue
			}
			if err := apiSSEWrite(w, event); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		case <-ticker.C():
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		}
	}
}

func (l *APISSELimiter) acquire(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Counts == nil {
		l.Counts = make(map[string]int)
	}
	if l.Counts[token] >= APISSEStreamsPerToken {
		return false
	}
	l.Counts[token]++
	return true
}

func (l *APISSELimiter) release(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Counts[token] <= 1 {
		delete(l.Counts, token)
		return
	}
	l.Counts[token]--
}

func apiSSELastID(r *http.Request) (uint64, bool, error) {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("last_event_id")
	}
	if value == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == math.MaxUint64 && errors.Is(err, strconv.ErrRange) {
		return 0, true, errors.New("invalid event id")
	}
	return id, true, nil
}

func apiSSEWrite(w http.ResponseWriter, event APIEvent) error {
	data := event.Data
	if !json.Valid(data) {
		data = json.RawMessage(`null`)
	}
	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data)
	return err
}

// authRecheckInterval is a small local copy (sprint 0 L4 escape rule:
// "small local copy ... report with file:line") of the constant that moved to
// hub/gate.go with ws_serve_revoke.go (ws_serve_revoke.go:9 at HEAD). The SSE
// revocation ticker must tick at the same interval as the WS watch.
const authRecheckInterval = 15 * time.Second

func APISSERevocation(ctx context.Context, principal Principal) <-chan struct{} {
	revoked := make(chan struct{})
	factory := principal.TickerFactory()
	if factory == nil {
		factory = RealTicker
	}
	go func() {
		ticker := factory(authRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C():
				if !StillValid(principal, now) {
					close(revoked)
					return
				}
			}
		}
	}()
	return revoked
}

func apiEventsRoute() APIRoute {
	return APIRoute{
		Method: http.MethodGet, Pattern: "/sessions/{id}/events", Scope: "read",
		Summary: "Stream session events", OperationID: "streamSessionEvents",
		Handler: apiReadPlaceholder,
		Params: []APIParam{
			{Name: "id", In: "path", Required: true, Description: "Session ID"},
			{Name: "Last-Event-ID", In: "header", Description: "Resume after event ID"},
			{Name: "last_event_id", In: "query", Description: "Resume after event ID"},
		},
		ResponseDescription: "Session event stream",
	}
}
