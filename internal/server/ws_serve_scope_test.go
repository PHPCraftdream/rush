package server

import (
	"testing"
)

func TestServeWSMCPStateReceivePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		auth    *clientAuth
		allowed bool
	}{
		{name: "ui", allowed: true},
		{name: "read", auth: newClientAuth(Principal{Scopes: []string{"read"}}, CommandPolicy{AllowAdmin: true})},
		{name: "admin listener disabled", auth: newClientAuth(Principal{Scopes: []string{"admin"}}, CommandPolicy{})},
		{name: "admin", auth: newClientAuth(Principal{Scopes: []string{"admin"}}, CommandPolicy{AllowAdmin: true}), allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{auth: tc.auth}
			if got := client.mayReceive(EventMCPState); got != tc.allowed {
				t.Fatalf("mayReceive(mcp_state) = %v, want %v", got, tc.allowed)
			}
			if !client.mayReceive(EventSessionCreated) {
				t.Fatal("non-MCP event was filtered")
			}
		})
	}
}

func TestEnvelopeType(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"broadcast shape", `{"type":"mcp_state","payload":{"a":1}}`, EventMCPState},
		{"id first", `{"id":"1","type":"mcp_state"}`, EventMCPState},
		{"type with escape uses the decoder", `{"type":"mcp_state"}`, EventMCPState},
		{"payload containing the key", `{"payload":{"type":"mcp_state"},"type":"x"}`, "x"},
		{"not json", `nope`, ""},
		{"empty", ``, ""},
		{"no type", `{"payload":1}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := envelopeType([]byte(tc.raw)); got != tc.want {
				t.Fatalf("envelopeType(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
