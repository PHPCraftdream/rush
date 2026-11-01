package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestResponseWriterStreamsFlushBeforeHandlerReturns(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	release := make(chan struct{})
	handlerTimedOut := make(chan struct{}, 1)
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Burst: 10})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: ready\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			select {
			case handlerTimedOut <- struct{}{}:
			default:
			}
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req) //nolint:bodyclose // Closed by the deferred Close below; the body is also read in a goroutine.
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			select {
			case data <- "read error: " + readErr.Error():
			default:
			}
			return
		}
		select {
		case data <- line:
		default:
		}
	}()
	select {
	case got := <-data:
		if !strings.HasPrefix(got, "event: ready") {
			t.Fatalf("unexpected stream data %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("flush did not reach client before handler return")
	case <-handlerTimedOut:
		t.Fatal("streaming handler timed out waiting for release")
	}
}

func TestAuditResponseWriterStatusBytesAndOptionalInterfaces(t *testing.T) {
	isolateAPIGlobals(t)
	cases := []struct {
		name   string
		handle func(http.ResponseWriter)
		status int
		bytes  int64
	}{
		{"implicit", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "ok") }, 200, 2},
		{"explicit", func(w http.ResponseWriter) { w.WriteHeader(201); _, _ = io.WriteString(w, "yes") }, 201, 3},
		{"write-only", func(w http.ResponseWriter) { _, _ = io.WriteString(w, "body") }, 200, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := httptest.NewRecorder()
			tracked := &statusWriter{ResponseWriter: base, status: 200}
			tc.handle(tracked)
			if tracked.status != tc.status || tracked.bytes != tc.bytes {
				t.Fatalf("status=%d bytes=%d", tracked.status, tracked.bytes)
			}
			if tracked.Unwrap() != base {
				t.Fatal("Unwrap did not return the underlying writer")
			}
			if _, ok := any(tracked).(http.Flusher); !ok {
				t.Fatal("missing Flusher")
			}
			if _, ok := any(tracked).(http.Hijacker); !ok {
				t.Fatal("missing Hijacker")
			}
		})
	}
}

func TestAuditRecordsScopeSessionAndPanic(t *testing.T) {
	isolateAPIGlobals(t)
	var captured strings.Builder
	logger := slog.New(slog.NewTextHandler(&captured, nil))
	panicHandler := auditMiddleware(logger, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		RecordScope(r.Context(), "run")
		RecordSession(r.Context(), "session-1")
		panic("controlled")
	}))
	func() {
		defer func() { _ = recover() }()
		panicHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/private", nil))
	}()
	line := captured.String()
	for _, expected := range []string{"scope=run", "session_id=session-1", "status=500", "route=unmatched"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("audit missing %q: %s", expected, line)
		}
	}
}

func TestBackoffDoublesWithoutSuccessResetAndCaps(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(1, 0)
	b := newFailureBackoff(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		b.Failure("192.0.2.1", now)
	}
	wants := []time.Duration{time.Second}
	for _, want := range wants {
		if got := b.Blocked("192.0.2.1", now); got != want {
			t.Fatalf("initial retry: got %s want %s", got, want)
		}
	}
	for offense, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		b.Failure("192.0.2.1", now)
		if got := b.Blocked("192.0.2.1", now); got != want {
			t.Fatalf("offense %d: got %s want %s", offense+2, got, want)
		}
	}
	b.mu.Lock()
	entryCount := len(b.entries)
	b.mu.Unlock()
	if entryCount != 1 {
		t.Fatalf("backoff entry unexpectedly evicted: %d", entryCount)
	}
}

func TestBodyCapRejectsKnownOversizeBeforeHandler(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	called := false
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, MaxBody: 2})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("large"))
	r.Header.Set("Authorization", "Bearer rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if called || w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != tooLargeBody {
		t.Fatalf("called=%v status=%d body=%q", called, w.Code, w.Body.String())
	}
}

func TestRecordScopeOutsideAuditIsSafe(t *testing.T) {
	isolateAPIGlobals(t)
	RecordScope(context.Background(), "read")
	RecordSession(context.Background(), "not-recorded")
}

func TestRedactExactTokenFormat(t *testing.T) {
	isolateAPIGlobals(t)
	token := "rush_12345678." + strings.Repeat("a", 43)
	if got, want := Redact(token), "rush_12345678.[REDACTED]"; got != want {
		t.Fatalf("Redact(%q) = %q, want %q", token, got, want)
	}
}
