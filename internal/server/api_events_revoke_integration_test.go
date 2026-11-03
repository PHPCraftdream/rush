package server

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type revokeIntegrationTicker struct{ ch chan time.Time }

func (t revokeIntegrationTicker) C() <-chan time.Time { return t.ch }
func (revokeIntegrationTicker) Stop()                 {}

func TestServeAPIEventsProductionListenerRevokesToken(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	ticker := revokeIntegrationTicker{ch: make(chan time.Time, 1)}
	previousTicker := serveAuthTickerFactory
	serveAuthTickerFactory = func(time.Duration) Ticker { return ticker }
	t.Cleanup(func() { serveAuthTickerFactory = previousTicker })
	store := config.GlobalAPITokenStore()
	token, record, err := store.CreateAPIToken("sse-revoke-integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := app.Sessions.Create(t.Context(), "revocation integration")
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	server := NewWithPolicy(app, policy, nil)
	baseURL := startPolicyServer(t, server)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/sessions/"+sess.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	lineCh := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		close(lineCh)
	}()
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				t.Fatal("SSE stream ended before revocation frame")
			}
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "event: ") {
				break
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SSE stream did not emit initial response")
		}
		break
	}
	if err := store.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	ticker.ch <- time.Now()
	seenClosed := false
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !seenClosed {
		select {
		case line, ok := <-lineCh:
			if !ok {
				t.Fatal("SSE stream ended without closed frame")
			}
			if line == "event: closed" {
				seenClosed = true
			}
		case <-timer.C:
			t.Fatal("timed out waiting for closed frame after injected revocation tick")
		}
	}
}
