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

func TestOpenAPIContractAndDeterminism(t *testing.T) {
	routes := APIRoutes()
	first, err := json.Marshal(BuildOpenAPIDocument(routes))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(BuildOpenAPIDocument(routes))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("OpenAPI output is not deterministic")
	}
	var document map[string]any
	if err := json.Unmarshal(first, &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"openapi", "info", "paths", "components"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	paths := document["paths"].(map[string]any)
	if len(paths) != len(routes) {
		t.Fatalf("paths=%d routes=%d", len(paths), len(routes))
	}
	for _, route := range routes {
		operation := paths[route.Pattern].(map[string]any)[lowercaseASCII(route.Method)].(map[string]any)
		if operation["operationId"] != route.OperationID || operation["x-rush-scope"] != route.Scope {
			t.Fatalf("contract mismatch: %+v", route)
		}
		responses := operation["responses"].(map[string]any)
		for _, status := range []string{"401", "403", "404", "405", "429", "500"} {
			if _, ok := responses[status]; !ok {
				t.Fatalf("missing response %s", status)
			}
		}
		if route.Pattern == "/health" && !reflect.DeepEqual(operation["security"], []any{}) {
			t.Fatal("health security must be empty")
		}
	}
}

func TestAPIProductionRoutesIntegration(t *testing.T) {
	const plaintextToken = "known-plaintext-token-must-not-be-logged"
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	api := NewAPIHandler(APIRoutes())
	// Inject the authenticated principal at the same boundary as Chain; the
	// production API handler performs routing and scope authorization.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: []string{"read"}}))
		}
		api.ServeHTTP(w, r)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 10 * time.Second}
	for _, path := range []string{"/api/v1/health", "/api/v1/version", "/api/v1/openapi.json"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
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
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d body=%q", path, response.StatusCode, body)
		}
		if path == "/api/v1/health" && string(body) != "{\"status\":\"ok\"}\n" {
			t.Fatalf("health body=%q", body)
		}
		if path == "/api/v1/version" || path == "/api/v1/openapi.json" {
			var value any
			if err := json.Unmarshal(body, &value); err != nil {
				t.Fatalf("%s response is not JSON: %v", path, err)
			}
		}
	}
	if strings.Contains(logs.String(), plaintextToken) {
		t.Fatal("integration logs contain plaintext token")
	}
}
