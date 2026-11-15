package server

import (
	"bytes"
	"encoding/json"
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

const configEnvelopeWithPaths = `{"id":"7","type":"config","payload":{"models":{},"providers":{},"cwd":"/home/me/project","contextPaths":["/home/me/AGENTS.md"],"skillsPaths":["/home/me/skills"],"theme":"dark","version":"1"}}`

func filterConfig(t *testing.T, auth *clientAuth, raw string) map[string]json.RawMessage {
	t.Helper()
	out := auth.filter([]byte(raw), newLazyMeta([]byte(raw)))
	if out == nil {
		return nil
	}
	var envelope struct {
		ID      string                     `json:"id"`
		Type    string                     `json:"type"`
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("filtered envelope is not JSON: %v: %s", err, out)
	}
	if envelope.ID != "7" || envelope.Type != EventConfig {
		t.Fatalf("envelope id/type changed: %+v", envelope)
	}
	return envelope.Payload
}

func TestConfigEventLocalPathsRequireConfigScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scopes    []string
		wantPaths bool
	}{
		{"read", []string{"read"}, false},
		{"run", []string{"run"}, false},
		{"no scopes", nil, false},
		{"unknown scope", []string{"root"}, false},
		{"config", []string{"config"}, true},
		{"admin", []string{"admin"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := filterConfig(t, newClientAuth(Principal{Scopes: tc.scopes}, CommandPolicy{AllowAdmin: true}), configEnvelopeWithPaths)
			if payload == nil {
				t.Fatal("config event was dropped")
			}
			for _, key := range configLocalPathKeys {
				if _, present := payload[key]; present != tc.wantPaths {
					t.Errorf("scopes %v: %q present = %v, want %v", tc.scopes, key, present, tc.wantPaths)
				}
			}
			for _, key := range []string{"models", "providers", "theme", "version"} {
				if _, present := payload[key]; !present {
					t.Errorf("scopes %v: non-path field %q was removed", tc.scopes, key)
				}
			}
		})
	}
}

func TestConfigEventRedactionFailsClosedAndLeavesOtherEventsAlone(t *testing.T) {
	read := newClientAuth(Principal{Scopes: []string{"read"}}, CommandPolicy{})
	for _, bad := range []string{`{"type":"config","payload":"cwd"}`, `{"type":"config","payload":[1]}`} {
		if out := read.filter([]byte(bad), newLazyMeta([]byte(bad))); out != nil {
			t.Errorf("unparseable config payload leaked: %s", out)
		}
	}
	noPayload := `{"id":"7","type":"config"}`
	if out := read.filter([]byte(noPayload), newLazyMeta([]byte(noPayload))); !bytes.Equal(out, []byte(noPayload)) {
		t.Errorf("config envelope without payload changed: %s", out)
	}
	other := `{"type":"session_created","payload":{"cwd":"/home/me/keep"}}`
	if out := read.filter([]byte(other), newLazyMeta([]byte(other))); !bytes.Equal(out, []byte(other)) {
		t.Errorf("non-config event was modified: %s", out)
	}
}

// TestConfigWireReplyHidesLocalPathsFromReadToken drives the real ConfigWire
// through Client.reply (the get_config path), so the redacted keys cannot
// drift from the struct's JSON names.
func TestConfigWireReplyHidesLocalPathsFromReadToken(t *testing.T) {
	wire := ConfigWire{
		Models:       map[string]ModelEntryWire{},
		Providers:    map[string]ProviderWire{},
		ContextPaths: []string{"/home/me/AGENTS.md"},
		SkillsPaths:  []string{"/home/me/skills"},
		CWD:          "/home/me/project",
		Theme:        "dark",
	}
	for _, tc := range []struct {
		scopes    []string
		wantPaths bool
	}{{[]string{"read"}, false}, {[]string{"config"}, true}} {
		client := &Client{send: make(chan []byte, 1), auth: newClientAuth(Principal{Scopes: tc.scopes}, CommandPolicy{})}
		client.reply("1", EventConfig, wire, "")
		var body []byte
		select {
		case body = <-client.send:
		default:
			t.Fatalf("scopes %v: no reply queued", tc.scopes)
		}
		for _, leaked := range []string{"/home/me/project", "/home/me/AGENTS.md", "/home/me/skills"} {
			if got := bytes.Contains(body, []byte(leaked)); got != tc.wantPaths {
				t.Errorf("scopes %v: %q present = %v, want %v\n%s", tc.scopes, leaked, got, tc.wantPaths, body)
			}
		}
		if !bytes.Contains(body, []byte(`"theme":"dark"`)) {
			t.Errorf("scopes %v: theme missing: %s", tc.scopes, body)
		}
	}
}
