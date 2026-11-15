package server

import (
	"context"
	"net/http"
	"testing"
)

func TestServeOriginAllowed(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		allowed []string
		want    bool
	}{
		{"empty", "", nil, true},
		{"null", "null", []string{"null"}, false},
		{"exact", "https://Example.com:443", []string{"https://example.com"}, true},
		{"scheme", "http://example.com", []string{"https://example.com"}, false},
		{"port", "https://example.com:444", []string{"https://example.com"}, false},
		{"slash", "https://example.com/", []string{"https://example.com"}, false},
		{"userinfo", "https://u@example.com", []string{"https://example.com"}, false},
		{"path", "https://example.com/path", []string{"https://example.com"}, false},
		{"ipv6", "http://[::1]:80", []string{"http://[::1]"}, true},
		{"default empty", "http://localhost:8080", nil, false},
		{"wildcard", "https://example.com", []string{"*"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveOriginAllowed(tc.origin, tc.allowed); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateAllowedOriginsRejectsWildcard(t *testing.T) {
	if err := validateAllowedOrigins([]string{"*"}); err == nil {
		t.Fatal("wildcard accepted")
	}
}

func TestBearerSubprotocolParsing(t *testing.T) {
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost/ws", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Add("Sec-WebSocket-Protocol", "rush.v1")
	r.Header.Add("Sec-WebSocket-Protocol", "rush.bearer.rush_12345678.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-abc")
	if !websocketUpgrade(r) || !offeredSubprotocol(r, "rush.v1") || !hasBearerSubprotocol(r) {
		t.Fatal("subprotocol list not recognized")
	}
}
