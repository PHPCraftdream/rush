package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type fakeUsageTicker struct {
	ch      chan time.Time
	stopped chan struct{}
}

func (t *fakeUsageTicker) C() <-chan time.Time { return t.ch }
func (t *fakeUsageTicker) Stop() {
	select {
	case <-t.stopped:
	default:
		close(t.stopped)
	}
}

type fakeUsageFlusher struct {
	calls int
	errs  []error
	done  chan struct{}
}

func (f *fakeUsageFlusher) Flush() error {
	f.calls++
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	if f.done != nil {
		select {
		case f.done <- struct{}{}:
		default:
		}
	}
	return err
}

func TestChainSuccessDoesNotResetFailureHistory(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(100, 0)
	verifier := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: verifier, Clock: func() time.Time { return now }}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := func(success bool) *httptest.ResponseRecorder {
		verifier.ok = success
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.10:1234"
		if success {
			r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 9; i++ {
		if got := req(false); got.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d status=%d", i, got.Code)
		}
	}
	if got := req(true); got.Code != http.StatusOK {
		t.Fatalf("successful auth status=%d", got.Code)
	}
	if got := req(false); got.Code != http.StatusUnauthorized {
		t.Fatalf("tenth failure status=%d", got.Code)
	}
	blocked := req(false)
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") != "1" {
		t.Fatalf("backoff status=%d retry=%q", blocked.Code, blocked.Header().Get("Retry-After"))
	}
}

func TestRunUsageFlusherTickerErrorsCancellationAndStop(t *testing.T) {
	isolateAPIGlobals(t)
	tick := &fakeUsageTicker{ch: make(chan time.Time, 1), stopped: make(chan struct{})}
	token := "rush_12345678." + strings.Repeat("a", 43)
	flush := &fakeUsageFlusher{errs: []error{errors.New("temporary " + token)}, done: make(chan struct{}, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logRecords := &captureHandler{notify: make(chan struct{}, 1)}
	old := slog.Default()
	slog.SetDefault(slog.New(logRecords))
	t.Cleanup(func() {
		slog.SetDefault(old)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunUsageFlusher(ctx, flush, time.Second, func(time.Duration) Ticker { return tick })
	}()
	for i := 0; i < 2; i++ {
		select {
		case tick.ch <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatal("timed out sending fake tick")
		}
		select {
		case <-flush.done:
		case <-time.After(10 * time.Second):
			t.Fatal("flusher did not handle tick")
		}
		if i == 0 {
			records := waitForRecords(t, logRecords, 1)
			record := records[0]
			if record.message != "API token usage flush failed" || !strings.Contains(record.attrs["error"].(string), "temporary") || strings.Contains(record.attrs["error"].(string), token) {
				t.Fatalf("unexpected or secret-bearing flush log: %#v", record)
			}
		}
	}
	if got := len(logRecords.snapshot()); got != 1 {
		t.Fatalf("flush logs=%v", logRecords.snapshot())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("flusher did not stop on cancellation")
	}
	select {
	case <-tick.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("ticker was not stopped")
	}
	if flush.calls != 2 {
		t.Fatalf("flush calls=%d", flush.calls)
	}
}

type verifierWithFlush struct {
	*fakeVerifier
	calls int
}

func (v *verifierWithFlush) Flush() error {
	v.calls++
	return nil
}

func TestChainDoesNotFlushUsageOnRequestPath(t *testing.T) {
	isolateAPIGlobals(t)
	v := &verifierWithFlush{fakeVerifier: &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if v.calls != 0 {
		t.Fatalf("request path flushed usage %d times", v.calls)
	}
}
