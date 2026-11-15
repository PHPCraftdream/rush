package server

import (
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestIsLoopbackHostIsLexical(t *testing.T) {
	if !IsLoopbackHost("localhost") {
		t.Fatal("localhost should be lexically classified as loopback")
	}
	if IsLoopbackHost("example.invalid") {
		t.Fatal("non-localhost hostname must not be classified as loopback")
	}
}

func TestNewWithPolicyCopiesSlices(t *testing.T) {
	surfaces := []ListenerSurface{SurfaceREST, SurfaceWS}
	origins := []string{"https://allowed.example", "https://also-allowed.example"}
	tokens := []string{"token-id-one", "token-id-two"}
	wantSurfaces := []ListenerSurface{SurfaceREST, SurfaceWS}
	wantOrigins := []string{"https://allowed.example", "https://also-allowed.example"}
	wantTokens := []string{"token-id-one", "token-id-two"}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: surfaces, AllowedOrigins: origins, TokenIDs: tokens, Auth: DenyAllAuth}
	server := NewWithPolicy(nil, policy, nil)

	// Change every element in the caller-owned backing arrays after construction.
	surfaces[0], surfaces[1] = SurfaceUI, SurfaceUI
	origins[0], origins[1] = "https://attacker.example", "https://also-attacker.example"
	tokens[0], tokens[1] = "changed-one", "changed-two"

	if !reflect.DeepEqual(server.policy.Surfaces, wantSurfaces) {
		t.Fatalf("server surfaces = %v, want original snapshot %v", server.policy.Surfaces, wantSurfaces)
	}
	if !reflect.DeepEqual(server.policy.AllowedOrigins, wantOrigins) {
		t.Fatalf("server allowed origins = %v, want original snapshot %v", server.policy.AllowedOrigins, wantOrigins)
	}
	if !reflect.DeepEqual(server.policy.TokenIDs, wantTokens) {
		t.Fatalf("server token IDs = %v, want original snapshot %v", server.policy.TokenIDs, wantTokens)
	}

	baseURL := startPolicyServer(t, server)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, test := range []struct {
		path string
		want int
	}{
		{path: "/api/v1/snapshot", want: http.StatusUnauthorized},
		{path: "/ws", want: http.StatusUnauthorized},
		{path: "/", want: http.StatusNotFound},
	} {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+test.path, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", test.path, err)
		}
		response.Body.Close()
		if response.StatusCode != test.want {
			t.Errorf("GET %s status = %d, want %d from original surface snapshot", test.path, response.StatusCode, test.want)
		}
	}
}

func TestResolvedLocalhostRejectsAnyNonLoopback(t *testing.T) {
	policy := ListenerPolicy{Address: "localhost:80", resolver: func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}, nil
	}}
	if _, err := policy.ResolvedAddress(); err == nil {
		t.Fatal("mixed loopback/non-loopback result accepted")
	}
}
