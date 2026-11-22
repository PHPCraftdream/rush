package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func serveAPITestStore(t *testing.T) *config.APITokenStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(root, "global"))
	return config.GlobalAPITokenStore()
}

func TestNewServeAPIRestSurfaceAndWSOnly(t *testing.T) {
	store := serveAPITestStore(t)
	policy := ListenerPolicy{Surfaces: []ListenerSurface{SurfaceWS}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Stop(context.Background())
	api.Configure(&policy)
	s := NewWithPolicy(nil, ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceWS}, Auth: policy.Auth, RESTHandler: policy.RESTHandler}, nil)
	baseURL := startPolicyServer(t, s)
	client := &http.Client{Timeout: 10 * time.Second}
	requestCtx, cancelRequest := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("ws-only REST status = %d, want 404", response.StatusCode)
	}

	policy.Surfaces = []ListenerSurface{SurfaceREST}
	api2, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer api2.Stop(context.Background())
	policy.Auth, policy.RESTHandler = nil, nil
	api2.Configure(&policy)
	s2 := NewWithPolicy(nil, ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: policy.Auth, RESTHandler: policy.RESTHandler}, nil)
	baseURL = startPolicyServer(t, s2)
	requestCtx, cancelRequest = context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelRequest()
	request, err = http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("REST health status = %d, want 200", response.StatusCode)
	}
}

func TestNewServeAPIHealthRateLimitPerIP(t *testing.T) {
	store := serveAPITestStore(t)
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	previousNow := serveAPINow
	serveAPINow = func() time.Time { return now }
	t.Cleanup(func() { serveAPINow = previousNow })
	policy := ListenerPolicy{Surfaces: []ListenerSurface{SurfaceREST}, Limits: ListenerLimits{RPS: 1, Burst: 1}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Stop(context.Background())
	request := func(remote string) *httptest.ResponseRecorder {
		t.Helper()
		requestCtx, cancelRequest := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancelRequest()
		req := httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/api/v1/health", nil)
		req.RemoteAddr = remote
		response := httptest.NewRecorder()
		api.RESTHandler.ServeHTTP(response, req)
		return response
	}
	if got := request("192.0.2.1:1"); got.Code != http.StatusOK {
		t.Fatalf("initial request status = %d, want 200", got.Code)
	}
	limited := request("192.0.2.1:2")
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "1" || limited.Header().Get("Content-Type") != "application/problem+json" || limited.Body.String() != authBusyBody {
		t.Fatalf("limited response = %d retry=%q content-type=%q body=%q", limited.Code, limited.Header().Get("Retry-After"), limited.Header().Get("Content-Type"), limited.Body.String())
	}
	if got := request("192.0.2.2:1"); got.Code != http.StatusOK {
		t.Fatalf("second IP status = %d, want 200", got.Code)
	}
	now = now.Add(time.Second)
	if got := request("192.0.2.1:1"); got.Code != http.StatusOK {
		t.Fatalf("request after exact refill status = %d, want 200", got.Code)
	}
}

func TestNewServeAPIChainEnforcesTokenAllowList(t *testing.T) {
	store := serveAPITestStore(t)
	token, record, err := store.CreateAPIToken("serve-builder", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Surfaces: []ListenerSurface{SurfaceREST}, TokenIDs: []string{"not-" + record.ID}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Stop(context.Background())
	requestCtx, cancelRequest := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelRequest()
	req := httptest.NewRequestWithContext(requestCtx, http.MethodGet, "/api/v1/version", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	api.RESTHandler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("allow-list status = %d, want 401", response.Code)
	}
}

func TestNewServeAPIUsesOneFlusherPerStore(t *testing.T) {
	store := serveAPITestStore(t)
	first, err := NewServeAPI(ListenerPolicy{}, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewServeAPI(ListenerPolicy{}, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	serveFlushers.Lock()
	shared := serveFlushers.stores[store]
	serveFlushers.Unlock()
	if shared == nil {
		t.Fatal("no shared flusher registered")
	}
	if shared.refs != 2 {
		t.Fatalf("shared flusher refs=%d, want two owners", shared.refs)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	serveFlushers.Lock()
	stillShared := serveFlushers.stores[store] == shared
	serveFlushers.Unlock()
	if !stillShared {
		t.Fatal("stopping one builder stopped the shared flusher")
	}
	if err := second.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServeAPIStopJoinsUsageFlusher(t *testing.T) {
	store := serveAPITestStore(t)
	api, err := NewServeAPI(ListenerPolicy{}, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := api.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
