package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
)

func TestServeAPIRealListenerIntegration(t *testing.T) {
	isolateAPIGlobals(t)
	store := config.GlobalAPITokenStore()
	const secretMarker = "plaintext-secret-scan-marker"
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := app.Sessions.Create(t.Context(), "integration")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Sessions.Create(t.Context(), "integration second"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := app.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}}); err != nil {
			t.Fatal(err)
		}
	}
	token, record, err := store.CreateAPIToken("integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked, revokedRecord, err := store.CreateAPIToken("revoked", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIToken(revokedRecord.ID); err != nil {
		t.Fatal(err)
	}
	expiredAt := time.Now().Add(-time.Minute)
	expired, _, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &expiredAt)
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
	server := NewWithPolicy(app, policy, nil)
	baseURL := startPolicyServer(t, server)
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(method, uri, bearer, requestID string) (serveAPIResult, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, baseURL+uri, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if requestID != "" {
			req.Header.Set("X-Request-Id", requestID)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(uri, "/api/v1/") && resp.Header.Get("X-Request-Id") == "" {
			t.Errorf("%s response omitted X-Request-Id", uri)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		secretPart := token[strings.IndexByte(token, '.')+1:]
		if strings.Contains(string(body), token) || strings.Contains(string(body), secretPart) {
			t.Errorf("%s response contains token material", uri)
		}
		return serveAPIResult{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}, body
	}

	unauthorized, unauthBody := request(http.MethodGet, "/api/v1/version", "", "")
	if unauthorized.StatusCode != http.StatusUnauthorized || string(unauthBody) != authFailureBody || unauthorized.Header.Get("Content-Type") != "application/problem+json" || unauthorized.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("no-token response = %d, %q, headers=%v", unauthorized.StatusCode, unauthBody, unauthorized.Header)
	}
	if unauthorized.Header.Get("X-Request-Id") == "" {
		t.Fatal("no-token response omitted X-Request-Id")
	}
	for _, invalid := range []string{revoked, expired} {
		resp, body := request(http.MethodGet, "/api/v1/version", invalid, "")
		if resp.StatusCode != unauthorized.StatusCode || string(body) != string(unauthBody) || resp.Header.Get("Content-Type") != unauthorized.Header.Get("Content-Type") || resp.Header.Get("WWW-Authenticate") != unauthorized.Header.Get("WWW-Authenticate") {
			t.Fatalf("invalid credential response differs: status=%d body=%q", resp.StatusCode, body)
		}
	}
	for _, queryKey := range []string{"token", "access_token"} {
		resp, body := request(http.MethodGet, "/api/v1/version?"+queryKey+"="+url.QueryEscape(token), token, "")
		if resp.StatusCode != unauthorized.StatusCode || string(body) != string(unauthBody) {
			t.Fatalf("%s query response = %d %q", queryKey, resp.StatusCode, body)
		}
	}
	for _, route := range []string{"/version", "/sessions", "/sessions/" + sess.ID, "/sessions/" + sess.ID + "/messages", "/models", "/openapi.json"} {
		resp, body := request(http.MethodGet, "/api/v1"+route, token, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d %s", route, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Request-Id") == "" {
			t.Errorf("GET %s omitted X-Request-Id", route)
		}
	}
	for _, route := range []string{"/sessions?limit=1", "/sessions/" + sess.ID + "/messages?limit=1"} {
		first, body := request(http.MethodGet, "/api/v1"+route, token, "")
		if first.StatusCode != http.StatusOK || !strings.Contains(string(body), `"next_after":"`) {
			t.Fatalf("first page %s = %d %s", route, first.StatusCode, body)
		}
		start := strings.Index(string(body), `"next_after":"`) + len(`"next_after":"`)
		end := strings.Index(string(body)[start:], `"`)
		cursor := string(body)[start : start+end]
		separator := "?"
		if strings.Contains(route, "?") {
			separator = "&"
		}
		second, secondBody := request(http.MethodGet, "/api/v1"+route+separator+"after="+url.QueryEscape(cursor), token, "")
		if second.StatusCode != http.StatusOK {
			t.Errorf("continuation %s = %d", route, second.StatusCode)
		}
		var firstPage, secondPage struct {
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &firstPage); err != nil {
			t.Fatalf("decode first page %s: %v", route, err)
		}
		if err := json.Unmarshal(secondBody, &secondPage); err != nil {
			t.Fatalf("decode continuation %s: %v", route, err)
		}
		if string(firstPage.Items) == string(secondPage.Items) {
			t.Errorf("continuation %s returned the same items", route)
		}
	}
	for _, tc := range []struct {
		uri string
		id  string
	}{{"/api/v1/version", "valid.request_id-1"}, {"/api/v1/version", "invalid id"}} {
		resp, _ := request(http.MethodGet, tc.uri, token, tc.id)
		got := resp.Header.Get("X-Request-Id")
		if tc.id == "valid.request_id-1" && got != tc.id {
			t.Errorf("valid request id = %q", got)
		}
		if tc.id == "invalid id" && got == tc.id {
			t.Errorf("invalid request id was echoed")
		}
	}
	health, healthBody := request(http.MethodGet, "/api/v1/health", "", "")
	if health.StatusCode != http.StatusOK || string(healthBody) != "{\"status\":\"ok\"}\n" {
		t.Errorf("health = %d %q", health.StatusCode, healthBody)
	}
	for _, route := range []string{"/", "/auth"} {
		resp, body := request(http.MethodGet, route, token, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d %q, want 404", route, resp.StatusCode, body)
		}
	}
	unknown, unknownBody := request(http.MethodGet, "/api/v1/not-a-route", token, "")
	if unknown.StatusCode != http.StatusNotFound || unknown.Header.Get("Content-Type") != "application/problem+json" || !strings.Contains(string(unknownBody), `"status":404`) {
		t.Errorf("unknown route = %d %q", unknown.StatusCode, unknownBody)
	}
	wrongMethod, wrongMethodBody := request(http.MethodPost, "/api/v1/version", token, "")
	if wrongMethod.StatusCode != http.StatusMethodNotAllowed || wrongMethod.Header.Get("Allow") != http.MethodGet || wrongMethod.Header.Get("Content-Type") != "application/problem+json" || !strings.Contains(string(wrongMethodBody), `"status":405`) {
		t.Errorf("wrong method = %d Allow=%q body=%q", wrongMethod.StatusCode, wrongMethod.Header.Get("Allow"), wrongMethodBody)
	}
	if record.ID == "" || strings.Contains(logs.String(), token) || strings.Contains(logs.String(), token[strings.IndexByte(token, '.')+1:]) || strings.Contains(logs.String(), secretMarker) {
		t.Fatal("token plaintext or secret appeared in captured logs")
	}
}

func TestServeAPIDoesNotServeRoutePlaceholders(t *testing.T) {
	isolateAPIGlobals(t)
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := app.Sessions.Create(t.Context(), "placeholder-check")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "placeholder-secret-scan-marker"
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("placeholder-check", []string{"read"}, "ask", nil)
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
	client := &http.Client{Timeout: 10 * time.Second}
	for _, route := range APIRoutes() {
		uri := path.Join("/api/v1", route.Pattern)
		uri = strings.ReplaceAll(uri, "{id}", sess.ID)
		uri = strings.ReplaceAll(uri, "{mid}", "missing-message")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, route.Method, baseURL+uri, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("%s %s read/close: %v/%v", route.Method, uri, readErr, closeErr)
		}
		if strings.Contains(string(body), token) || strings.Contains(string(body), token[strings.IndexByte(token, '.')+1:]) {
			t.Errorf("%s response contains token material", uri)
		}
		if resp.StatusCode == http.StatusNotImplemented {
			t.Errorf("%s %s served APIRoutes placeholder: %s", route.Method, uri, body)
		}
	}
	secretPart := token[strings.IndexByte(token, '.')+1:]
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), secretPart) || strings.Contains(logs.String(), marker) {
		t.Fatal("token plaintext or secret appeared in captured logs")
	}
}
