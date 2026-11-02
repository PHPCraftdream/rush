package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

func apiEventsRequest(ctx context.Context, lastID string) (*http.Request, *apiEventsFlushRecorder) {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil)
	r.SetPathValue("id", "s")
	r = r.WithContext(WithPrincipal(r.Context(), Principal{TokenID: "token", Scopes: []string{"read"}}))
	if lastID != "" {
		r.Header.Set("Last-Event-ID", lastID)
	}
	w := &apiEventsFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 8), heartbeat: make(chan struct{}, 1)}
	return r, w
}

// apiEventsBoundedRequest bounds synchronous handler calls so a regression that
// starts streaming fails the test instead of hanging it.
func apiEventsBoundedRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil)
	r.SetPathValue("id", id)
	return r
}

func awaitAPIEventsSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestAPIEventsHandlerTruncatedReplaySendsResetFirst(t *testing.T) {
	log := NewAPIEventLog(nil)
	log.maxPerSession = 2
	log.Publish("s", "message", map[string]string{"n": "1"})
	log.Publish("s", "message", map[string]string{"n": "2"})
	last, _ := log.Publish("s", "message", map[string]string{"n": "3"})
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r, w := apiEventsRequest(ctx, "0")
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, w.flushed, "initial flush")
	awaitAPIEventsSignal(t, w.flushed, "replay flush")
	cancel()
	awaitAPIEventsSignal(t, done, "handler exit")
	body := w.Body.String()
	reset := "id: 0\nevent: reset\ndata: {\"reason\":\"history_truncated\"}\n\n"
	if !strings.HasPrefix(body, "retry: 3000\n\n"+reset) || !strings.Contains(body, "id: "+strconvFormat(last.ID)+"\nevent: message\ndata: {\"n\":\"3\"}\n\n") {
		t.Fatalf("truncated replay frame order/content: %q", body)
	}
}

// publishOnFirstWrite publishes after the handler has subscribed but before it replays,
// so the event reaches the handler through both the replay and the live channel.
type publishOnFirstWrite struct {
	*apiEventsFlushRecorder
	once    sync.Once
	publish func()
}

func (w *publishOnFirstWrite) Write(data []byte) (int, error) {
	w.once.Do(w.publish)
	return w.apiEventsFlushRecorder.Write(data)
}

func TestAPIEventsHandlerDeduplicatesEventSeenInReplayAndLive(t *testing.T) {
	log := NewAPIEventLog(nil)
	log.Publish("s", "message", map[string]string{"n": "1"})
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, base := apiEventsRequest(ctx, "0")
	var racing APIEvent
	w := &publishOnFirstWrite{apiEventsFlushRecorder: base, publish: func() {
		racing, _ = log.Publish("s", "message", map[string]string{"n": "2"})
	}}
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, base.flushed, "initial flush")
	awaitAPIEventsSignal(t, base.flushed, "replay flush")
	sentinel, _ := log.Publish("s", "message", map[string]string{"n": "3"})
	awaitAPIEventsSignal(t, base.flushed, "sentinel flush")
	cancel()
	awaitAPIEventsSignal(t, done, "handler exit")
	body := base.Body.String()
	for _, id := range []uint64{1, racing.ID, sentinel.ID} {
		if frame := "id: " + strconvFormat(id) + "\nevent: message\n"; strings.Count(body, frame) != 1 {
			t.Fatalf("event %d must appear exactly once: %q", id, body)
		}
	}
}

func TestAPIEventsHandlerReplaysThenDeliversConcurrentLiveEventOnce(t *testing.T) {
	log := NewAPIEventLog(nil)
	first, _ := log.Publish("s", "message", map[string]string{"n": "1"})
	second, _ := log.Publish("s", "message", map[string]string{"n": "2"})
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r, w := apiEventsRequest(ctx, strconvFormat(first.ID))
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, w.flushed, "initial flush")
	awaitAPIEventsSignal(t, w.flushed, "replay flush")
	third, ok := log.Publish("s", "message", map[string]string{"n": "3"})
	if !ok {
		cancel()
		t.Fatal("live publish failed")
	}
	awaitAPIEventsSignal(t, w.flushed, "live flush")
	cancel()
	awaitAPIEventsSignal(t, done, "handler exit")
	body := w.Body.String()
	for _, id := range []uint64{second.ID, third.ID} {
		frame := "id: " + strconvFormat(id) + "\nevent: message\n"
		if strings.Count(body, frame) != 1 {
			t.Fatalf("event %d should appear once: %q", id, body)
		}
	}
	if strings.Contains(body, "id: "+strconvFormat(first.ID)+"\n") {
		t.Fatalf("Last-Event-ID event was replayed: %q", body)
	}
}

func TestAPIEventsHandlerRejectsForeignSessionBeforeStreaming(t *testing.T) {
	log := NewAPIEventLog(nil)
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}, foreign: true}, log, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil)
	r.SetPathValue("id", "s")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/problem+json" || w.Body.String() == "" {
		t.Fatalf("foreign session response = %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if log.SubscriberCount("s") != 0 {
		t.Fatal("foreign request subscribed before rejecting")
	}
}

func TestAPIEventsHandlerStreamLimitAndDisconnectReleaseSlot(t *testing.T) {
	log := NewAPIEventLog(nil)
	store := apiEventsTestStore{item: session.Session{ID: "s"}}
	limiter := &apiSSELimiter{}
	h := newAPIEventsHandler(store, log, limiter, nil)
	cancels := make([]context.CancelFunc, apiSSEStreamsPerToken)
	dones := make([]chan struct{}, apiSSEStreamsPerToken)
	for i := range cancels {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		r, w := apiEventsRequest(ctx, "")
		dones[i] = make(chan struct{})
		go func(done chan struct{}) { defer close(done); h(w, r) }(dones[i])
		awaitAPIEventsSignal(t, w.flushed, "stream start")
	}
	limitedCtx, limitedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer limitedCancel()
	r := httptest.NewRequestWithContext(limitedCtx, http.MethodGet, "/events", nil)
	r.SetPathValue("id", "s")
	r = r.WithContext(WithPrincipal(r.Context(), Principal{TokenID: "token", Scopes: []string{"read"}}))
	limited := httptest.NewRecorder()
	h(limited, r)
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "5" {
		t.Fatalf("ninth stream response = %d, Retry-After %q", limited.Code, limited.Header().Get("Retry-After"))
	}
	cancels[0]()
	awaitAPIEventsSignal(t, dones[0], "disconnected stream exit")
	ctx, cancel := context.WithCancel(context.Background())
	r2, w2 := apiEventsRequest(ctx, "")
	done2 := make(chan struct{})
	go func() { defer close(done2); h(w2, r2) }()
	awaitAPIEventsSignal(t, w2.flushed, "replacement stream start")
	cancel()
	awaitAPIEventsSignal(t, done2, "replacement stream exit")
	for i := 1; i < len(cancels); i++ {
		cancels[i]()
		awaitAPIEventsSignal(t, dones[i], "stream cleanup")
	}
	if len(limiter.counts) != 0 || log.SubscriberCount("s") != 0 {
		t.Fatalf("stream resources leaked: limiter=%v subscribers=%d", limiter.counts, log.SubscriberCount("s"))
	}
}

func TestAPIEventsHandlerSessionDeletionSendsClosed(t *testing.T) {
	log := NewAPIEventLog(nil)
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w := apiEventsRequest(ctx, "")
	done := make(chan struct{})
	go func() { defer close(done); h(w, r) }()
	awaitAPIEventsSignal(t, w.flushed, "stream start")
	log.DeleteSession("s")
	awaitAPIEventsSignal(t, done, "session deletion exit")
	if !strings.HasSuffix(w.Body.String(), "event: closed\ndata: {\"reason\":\"session_deleted\"}\n\n") {
		t.Fatalf("session deletion frame missing: %q", w.Body.String())
	}
}

func TestAPIEventsHandlerUnknownSessionDoesNotStartStream(t *testing.T) {
	log := NewAPIEventLog(nil)
	h := newAPIEventsHandler(apiEventsTestStore{getErr: sql.ErrNoRows}, log, nil, nil)
	r := apiEventsBoundedRequest(t, "missing")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusNotFound || log.SubscriberCount("missing") != 0 {
		t.Fatalf("missing session response/subscribers = %d/%d", w.Code, log.SubscriberCount("missing"))
	}
}
