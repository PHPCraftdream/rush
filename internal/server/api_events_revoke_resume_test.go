package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

type apiEventsResumeTicker struct{ ch chan time.Time }

func (t apiEventsResumeTicker) C() <-chan time.Time { return t.ch }
func (apiEventsResumeTicker) Stop()                 {}

type synchronizedSSELogCapture struct {
	mu sync.Mutex
	strings.Builder
}

func (capture *synchronizedSSELogCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.Builder.Write(data)
}

func (capture *synchronizedSSELogCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.Builder.String()
}

func TestServeAPIEventsProductionResumeDeliversBetweenRechecks(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	var logs synchronizedSSELogCapture
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	tickerMu := sync.Mutex{}
	tickerReady := make(chan struct{}, apiSSEStreamsPerToken)
	tickers := make([]apiEventsResumeTicker, 0, apiSSEStreamsPerToken)
	previousTicker := serveAuthTickerFactory
	serveAuthTickerFactory = func(time.Duration) Ticker {
		ticker := apiEventsResumeTicker{ch: make(chan time.Time, 1)}
		tickerMu.Lock()
		tickers = append(tickers, ticker)
		tickerMu.Unlock()
		select {
		case tickerReady <- struct{}{}:
		default:
		}
		return ticker
	}
	t.Cleanup(func() { serveAuthTickerFactory = previousTicker })
	previousRoutes := serveAPIRoutes
	var productionLog *APIEventLog
	serveAPIRoutes = func(app *appPkg.App, broker *APIPermissionBroker, autoApprove bool) ([]APIRoute, *APIEventLog, *APIRunRegistry) {
		routes, eventLog, registry := previousRoutes(app, broker, autoApprove)
		productionLog = eventLog
		return routes, eventLog, registry
	}
	t.Cleanup(func() { serveAPIRoutes = previousRoutes })

	store := config.GlobalAPITokenStore()
	token, record, err := store.CreateAPIToken("sse-resume-integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := app.Sessions.Create(t.Context(), "SSE resume integration")
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	streams := make([]*http.Response, 0, apiSSEStreamsPerToken)
	framesByStream := make([]<-chan apiEventsIntegrationFrame, 0, apiSSEStreamsPerToken)
	for range apiSSEStreamsPerToken {
		stream, frames := apiEventsIntegrationOpen(t, &http.Client{Timeout: 10 * time.Second}, baseURL+"/api/v1/sessions/"+sess.ID+"/events", token, "")
		if stream.StatusCode != http.StatusOK {
			stream.Body.Close()
			t.Fatalf("stream status = %d", stream.StatusCode)
		}
		streams = append(streams, stream) //nolint:bodyclose // Closed by the deferred loop below.
		framesByStream = append(framesByStream, frames)
	}
	for _, stream := range streams {
		defer stream.Body.Close()
	}
	for range apiSSEStreamsPerToken {
		select {
		case <-tickerReady:
		case <-time.After(5 * time.Second):
			t.Fatal("SSE stream did not initialize its revocation recheck")
		}
	}
	limited, _ := apiEventsIntegrationOpen(t, &http.Client{Timeout: 10 * time.Second}, baseURL+"/api/v1/sessions/"+sess.ID+"/events", token, "")
	if limited.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream beyond per-token maximum status = %d", limited.StatusCode)
	}
	limitedBody, readErr := io.ReadAll(limited.Body)
	limited.Body.Close()
	if readErr != nil || strings.Contains(string(limitedBody), token) {
		t.Fatalf("limited response read error=%v or leaked plaintext token", readErr)
	}
	if productionLog == nil {
		t.Fatal("production API constructor did not provide its event log")
	}
	// Publish through the event log returned by the production route constructor. This
	// deliberately happens without advancing the auth ticker: a recheck must not
	// monopolize the SSE delivery select or delay normal events until the next tick.
	if _, ok := productionLog.Publish(sess.ID, "resume_probe", map[string]string{"value": "before-recheck"}); !ok {
		t.Fatal("production event log rejected event")
	}
	frame := apiEventsIntegrationAwait(t, framesByStream[0], "resume_probe")
	if !strings.Contains(string(frame.data), "before-recheck") {
		t.Fatalf("event payload = %s", frame.data)
	}
	if err := store.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	tickerMu.Lock()
	for _, ticker := range tickers {
		ticker.ch <- time.Now()
	}
	tickerMu.Unlock()
	for index, frames := range framesByStream {
		closed := apiEventsIntegrationAwait(t, frames, "closed")
		if string(closed.data) != `{"reason":"token_revoked"}` {
			t.Fatalf("closed payload for stream %d = %s", index, closed.data)
		}
		if strings.Contains(logs.String(), token) || strings.Contains(string(closed.data), token) {
			t.Fatal("plaintext API token appeared in closed response or slog output")
		}
	}
	reopened, _ := apiEventsIntegrationOpen(t, &http.Client{Timeout: 10 * time.Second}, baseURL+"/api/v1/sessions/"+sess.ID+"/events", token, "")
	if reopened.StatusCode != http.StatusUnauthorized {
		reopened.Body.Close()
		t.Fatalf("revoked token was not denied after stream slot release: status = %d", reopened.StatusCode)
	}
	reopened.Body.Close()
	if strings.Contains(logs.String(), token) || strings.Contains(string(frame.data), token) {
		t.Fatal("plaintext API token appeared in SSE response or slog output")
	}
}
