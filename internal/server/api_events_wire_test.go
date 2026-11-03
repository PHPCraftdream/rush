package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

// Every writer layer between the handler and the connection must pass Flush
// down, including wrappers that do not implement http.Flusher themselves.
func TestAPIWriterLayersPassFlushDown(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracking := &apiTrackingWriter{ResponseWriter: recorder}
	audited := &statusWriter{ResponseWriter: tracking, status: http.StatusOK}
	if err := http.NewResponseController(audited).Flush(); err != nil {
		t.Fatalf("flush through statusWriter over apiTrackingWriter: %v", err)
	}
	if !recorder.Flushed {
		t.Fatal("the flush did not reach the underlying writer")
	}

	recorder = httptest.NewRecorder()
	tracking = &apiTrackingWriter{ResponseWriter: &statusWriter{ResponseWriter: recorder, status: http.StatusOK}}
	if err := http.NewResponseController(tracking).Flush(); err != nil {
		t.Fatalf("flush through apiTrackingWriter over statusWriter: %v", err)
	}
	if !recorder.Flushed {
		t.Fatal("the flush did not reach the underlying writer (reverse nesting)")
	}
}

// The first frame of an event stream reaches a client over the production
// listener (authentication chain and all writer wrappers) without waiting for
// an event or a heartbeat.
func TestServeAPIEventStreamDeliversFirstFrame(t *testing.T) {
	isolateAPIGlobals(t)
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := app.Sessions.Create(t.Context(), "sse-first-frame")
	if err != nil {
		t.Fatal(err)
	}
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("sse-first-frame", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/sessions/"+sess.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("no response headers within the bound: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The request context bounds the read, so a missing first frame fails the test.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "retry: 3000\n" {
		t.Fatalf("first line = %q (err %v), want the retry hint", line, err)
	}
}
