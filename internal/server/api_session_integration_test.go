package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestServeAPISessionWritesThroughProductionListener(t *testing.T) {
	isolateAPIGlobals(t)
	store := config.GlobalAPITokenStore()
	runToken, _, err := store.CreateAPIToken("session-run", []string{"run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	readToken, _, err := store.CreateAPIToken("session-read", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(app, policy, nil))
	client := &http.Client{Timeout: 10 * time.Second}
	type result struct {
		StatusCode int
		Header     http.Header
	}
	request := func(method, uri, token, idempotencyKey string, body io.Reader) (result, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, baseURL+uri, body)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close response: %v/%v", readErr, closeErr)
		}
		return result{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}, responseBody
	}
	decodeItems := func(body []byte) []json.RawMessage {
		t.Helper()
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode sessions list: %v: %s", err, body)
		}
		return page.Items
	}

	created, body := request(http.MethodPost, "/api/v1/sessions", runToken, "", strings.NewReader(`{"title":"created by API"}`))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", created.StatusCode, body)
	}
	var createdSession struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &createdSession); err != nil || createdSession.ID == "" {
		t.Fatalf("created session = %s, err=%v", body, err)
	}
	listed, body := request(http.MethodGet, "/api/v1/sessions", runToken, "", nil)
	if listed.StatusCode != http.StatusOK || len(decodeItems(body)) != 1 {
		t.Fatalf("list after create = %d %s", listed.StatusCode, body)
	}
	forked, body := request(http.MethodPost, "/api/v1/sessions/"+createdSession.ID+"/fork", runToken, "", strings.NewReader(`{"title":"forked"}`))
	if forked.StatusCode != http.StatusCreated {
		t.Fatalf("fork = %d %s", forked.StatusCode, body)
	}
	deleted, body := request(http.MethodDelete, "/api/v1/sessions/"+createdSession.ID, runToken, "", nil)
	if deleted.StatusCode != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("delete = %d %s", deleted.StatusCode, body)
	}
	listed, body = request(http.MethodGet, "/api/v1/sessions", runToken, "", nil)
	if listed.StatusCode != http.StatusOK || len(decodeItems(body)) != 1 {
		t.Fatalf("list after delete = %d %s, want only fork", listed.StatusCode, body)
	}

	first, firstBody := request(http.MethodPost, "/api/v1/sessions", runToken, "session-idempotency-once", strings.NewReader(`{"title":"idempotent"}`))
	second, secondBody := request(http.MethodPost, "/api/v1/sessions", runToken, "session-idempotency-once", strings.NewReader(`{"title":"idempotent"}`))
	if first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusCreated || second.Header.Get("Idempotent-Replayed") != "true" || string(firstBody) != string(secondBody) {
		t.Fatalf("idempotent responses = %d %s, %d %s replay=%q", first.StatusCode, firstBody, second.StatusCode, secondBody, second.Header.Get("Idempotent-Replayed"))
	}
	listed, body = request(http.MethodGet, "/api/v1/sessions", runToken, "", nil)
	if listed.StatusCode != http.StatusOK || len(decodeItems(body)) != 2 {
		t.Fatalf("list after replay = %d %s, want two sessions", listed.StatusCode, body)
	}

	readDenied, readBody := request(http.MethodPost, "/api/v1/sessions", readToken, "", strings.NewReader(`{}`))
	noToken, noTokenBody := request(http.MethodPost, "/api/v1/sessions", "", "", strings.NewReader(`{}`))
	invalidToken, invalidBody := request(http.MethodPost, "/api/v1/sessions", "invalid-token", "", strings.NewReader(`{}`))
	if readDenied.StatusCode != http.StatusForbidden {
		t.Fatalf("read token status = %d %s, want 403", readDenied.StatusCode, readBody)
	}
	if noToken.StatusCode != http.StatusUnauthorized || invalidToken.StatusCode != http.StatusUnauthorized || noToken.Header.Get("Content-Type") != invalidToken.Header.Get("Content-Type") || noToken.Header.Get("WWW-Authenticate") != invalidToken.Header.Get("WWW-Authenticate") || string(noTokenBody) != string(invalidBody) {
		t.Fatalf("unauthenticated responses differ: no-token=%d %s invalid=%d %s", noToken.StatusCode, noTokenBody, invalidToken.StatusCode, invalidBody)
	}
	for _, token := range []string{runToken, readToken} {
		if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), token[strings.IndexByte(token, '.')+1:]) {
			t.Fatal("plaintext token appeared in captured logs")
		}
	}
}
