package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

type apiEventsTestStore struct {
	item    session.Session
	getErr  error
	foreign bool
}

func (s apiEventsTestStore) GetSession(*http.Request, string) (session.Session, error) {
	return s.item, s.getErr
}
func (s apiEventsTestStore) OwnsSession(session.Session) bool { return !s.foreign }

type apiEventsFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed   chan struct{}
	heartbeat chan struct{}
}

func (w *apiEventsFlushRecorder) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if strings.Contains(string(data), ": keep-alive\n\n") {
		select {
		case w.heartbeat <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *apiEventsFlushRecorder) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

type apiEventsTestTicker struct {
	ch chan time.Time
}

func (t apiEventsTestTicker) C() <-chan time.Time { return t.ch }
func (apiEventsTestTicker) Stop()                 {}

func TestAPIEventsHandlerRejectsInvalidIDAndUnknownSessions(t *testing.T) {
	log := NewAPIEventLog(nil)
	limiter := &apiSSELimiter{}
	h := newAPIEventsHandler(apiEventsTestStore{getErr: sql.ErrNoRows}, log, limiter, nil)
	request := apiEventsBoundedRequest(t, "missing")
	response := httptest.NewRecorder()
	h(response, request)
	if response.Code != http.StatusNotFound || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("missing session response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}

	store := apiEventsTestStore{item: session.Session{ID: "s"}}
	h = newAPIEventsHandler(store, log, limiter, nil)
	request = apiEventsBoundedRequest(t, "s")
	request.Header.Set("Last-Event-ID", "1x")
	response = httptest.NewRecorder()
	h(response, request)
	if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("malformed ID response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestAPIEventsHandlerWritesRetryReplayAndHeartbeat(t *testing.T) {
	log := NewAPIEventLog(nil)
	first, _ := log.Publish("s", "message", map[string]string{"value": "one"})
	log.Publish("other", "message", map[string]string{"secret": "foreign"})
	ticker := apiEventsTestTicker{ch: make(chan time.Time, 1)}
	limiter := &apiSSELimiter{}
	h := newAPIEventsHandler(apiEventsTestStore{item: session.Session{ID: "s"}}, log, limiter, func() apiSSETicker { return ticker })
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/events?last_event_id=0", nil)
	request.SetPathValue("id", "s")
	request = request.WithContext(WithPrincipal(request.Context(), Principal{TokenID: "token", Scopes: []string{"read"}}))
	response := &apiEventsFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 1), heartbeat: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() { defer close(done); h(response, request) }()
	select {
	case <-response.flushed:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("handler did not flush start")
	}
	ticker.ch <- time.Now()
	select {
	case <-response.heartbeat:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("handler did not write heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not stop")
	}
	body := response.Body.String()
	want := "retry: 3000\n\nid: " + strconvFormat(first.ID) + "\nevent: message\ndata: {\"value\":\"one\"}\n\n: keep-alive\n\n"
	if body != want {
		t.Fatalf("SSE bytes\n got: %q\nwant: %q", body, want)
	}
	if strings.Contains(body, "foreign") {
		t.Fatal("stream included another session")
	}
	if response.Header().Get("Content-Type") != "text/event-stream" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("SSE headers = %v", response.Header())
	}
	if len(limiter.counts) != 0 {
		t.Fatalf("stream limiter leaked: %v", limiter.counts)
	}
}

func strconvFormat(value uint64) string { return fmt.Sprintf("%d", value) }
