package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenAPIBothDirections(t *testing.T) {
	routes := APIRoutes()
	first, err := json.Marshal(BuildOpenAPIDocument(routes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(BuildOpenAPIDocument(routes))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("OpenAPI bytes differ")
	}
	var doc map[string]any
	if err := json.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 4 {
		t.Fatalf("top-level keys=%v", doc)
	}
	for _, key := range []string{"openapi", "info", "paths", "components"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	want := map[string]bool{}
	for _, r := range routes {
		want[r.Method+" "+r.Pattern+" "+r.Scope] = true
	}
	seen := map[string]bool{}
	operations := map[string]bool{}
	paths := doc["paths"].(map[string]any)
	for path, item := range paths {
		for method, raw := range item.(map[string]any) {
			op := raw.(map[string]any)
			scope := op["x-rush-scope"].(string)
			key := strings.ToUpper(method) + " " + path + " " + scope
			id := op["operationId"].(string)
			if !want[key] {
				t.Fatalf("unexpected document operation %q id=%q", key, id)
			}
			seen[key] = true
			if operations[id] {
				t.Fatalf("duplicate operationId %s", id)
			}
			operations[id] = true
			security := op["security"]
			if scope == "none" {
				if !reflect.DeepEqual(security, []any{}) {
					t.Fatalf("security for %s: %#v", key, security)
				}
			} else if !reflect.DeepEqual(security, []any{map[string]any{"bearerAuth": []any{}}}) {
				t.Fatalf("security for %s: %#v", key, security)
			}
			responses := op["responses"].(map[string]any)
			for _, status := range []string{"401", "403", "404", "405", "429", "500"} {
				content := responses[status].(map[string]any)["content"].(map[string]any)
				schema := content["application/problem+json"].(map[string]any)["schema"].(map[string]any)
				if schema["$ref"] != "#/components/schemas/Problem" {
					t.Fatalf("%s response %s schema=%v", key, status, schema)
				}
			}
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("document set=%v expected=%v", seen, want)
	}
}

func TestAPIProductionRoutesScopeIntegration(t *testing.T) {
	const plaintextToken = "known-plaintext-token-must-not-be-logged"
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	api := NewAPIHandler(APIRoutes())
	// This boundary supplies the authenticated identity just as Chain does,
	// while all API routing and authorization remain in the production handler.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			scopes := []string{}
			if r.Header.Get("X-Test-Scopes") == "read" {
				scopes = []string{"read"}
			}
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: scopes}))
		}
		api.ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 10 * time.Second}

	for _, tc := range []struct {
		path, scopes string
		status       int
		body         string
	}{
		{"/api/v1/version", "read", http.StatusOK, ""},
		{"/api/v1/openapi.json", "read", http.StatusOK, ""},
		{"/api/v1/version", "", http.StatusForbidden, ""},
		{"/api/v1/health", "", http.StatusOK, "{\"status\":\"ok\"}\n"},
	} {
		path := tc.path
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.scopes != "" {
			req.Header.Set("X-Test-Scopes", tc.scopes)
		}
		req.Header.Set("Authorization", "Bearer "+plaintextToken)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			t.Fatalf("%s body read: %v", path, readErr)
		}
		if closeErr != nil {
			t.Fatalf("%s body close: %v", path, closeErr)
		}
		if response.StatusCode != tc.status {
			t.Fatalf("%s: status=%d body=%q", path, response.StatusCode, body)
		}
		if tc.status == http.StatusOK && (path == "/api/v1/version" || path == "/api/v1/openapi.json") {
			var value any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatalf("%s response is not JSON: %v", path, err)
			}
		}
		if tc.body != "" && string(body) != tc.body {
			t.Fatalf("%s body=%q", path, body)
		}
	}
	if strings.Contains(logs.String(), plaintextToken) {
		t.Fatal("integration logs contain plaintext token")
	}
}
