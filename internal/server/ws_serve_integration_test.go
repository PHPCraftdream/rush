package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsServeIntegrationLogBuffer struct {
	mu     sync.Mutex
	buffer strings.Builder
}

func (b *wsServeIntegrationLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *wsServeIntegrationLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestServeWSProductionAuthenticationIntegration(t *testing.T) {
	var logBuffer wsServeIntegrationLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	endpoint, token, _, _ := newServeWSFixture(t, 2)
	validProtocols := []string{"rush.v1", "rush.bearer." + token}
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, Subprotocols: validProtocols}
	conn, response, err := dialer.Dial(endpoint+"/ws", nil) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("subprotocol auth failed: %v (%v)", err, response)
	}
	if conn.Subprotocol() != "rush.v1" {
		t.Fatalf("selected subprotocol = %q", conn.Subprotocol())
	}
	conn.Close()

	// Multiple protocol header lines are accepted, and the selected protocol
	// remains the version marker rather than the credential-bearing value.
	multi := make(http.Header)
	multi.Add("Sec-WebSocket-Protocol", "rush.bearer."+token)
	multi.Add("Sec-WebSocket-Protocol", "rush.v1")
	conn, response, err = (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(endpoint+"/ws", multi) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("multi-header subprotocol auth failed: %v (%v)", err, response)
	}
	if conn.Subprotocol() != "rush.v1" {
		t.Fatalf("selected multi-header protocol = %q", conn.Subprotocol())
	}
	conn.Close()

	badAuth := make(http.Header)
	badAuth.Set("Authorization", "Bearer definitely-invalid-token")
	badSubprotocol := make(http.Header)
	badSubprotocol.Set("Sec-WebSocket-Protocol", "rush.v1, rush.bearer.definitely-invalid-token")
	missingVersion := make(http.Header)
	missingVersion.Set("Sec-WebSocket-Protocol", "rush.bearer."+token)
	ambiguous := make(http.Header)
	ambiguous.Set("Authorization", "Bearer "+token)
	ambiguous.Set("Sec-WebSocket-Protocol", "rush.v1, rush.bearer."+token)
	var badResponses []struct {
		status  int
		body    string
		headers http.Header
	}
	for i, headers := range []http.Header{badAuth, badSubprotocol, missingVersion, ambiguous} {
		_, resp, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(endpoint+"/ws", headers)
		if err == nil || resp == nil {
			t.Fatalf("bad credential unexpectedly connected: response=%v err=%v", resp, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		wantStatus := http.StatusUnauthorized
		if i >= 2 {
			wantStatus = http.StatusBadRequest
		}
		if resp.StatusCode != wantStatus || strings.Contains(string(body), token) {
			t.Fatalf("status/body = %d/%q, want %d without token", resp.StatusCode, body, wantStatus)
		}
		badResponses = append(badResponses, struct {
			status  int
			body    string
			headers http.Header
		}{resp.StatusCode, string(body), resp.Header.Clone()})
	}
	if badResponses[0].status != http.StatusUnauthorized || badResponses[0].status != badResponses[1].status || badResponses[0].body != badResponses[1].body || !equalHeaders(badResponses[0].headers, badResponses[1].headers) {
		t.Fatalf("bad-token responses differ: %#v vs %#v", badResponses[0], badResponses[1])
	}

	for _, tc := range []struct {
		name   string
		url    string
		header http.Header
		cookie bool
	}{
		{name: "query", url: "?token=" + token},
		{name: "cookie", cookie: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.header
			if tc.cookie {
				h = make(http.Header)
				h.Set("Cookie", cookieName+"="+token)
			}
			d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
			c, resp, err := d.Dial(endpoint+"/ws"+tc.url, h)
			if c != nil {
				c.Close()
			}
			if err == nil || resp == nil {
				t.Fatalf("expected rejected handshake: response=%v err=%v", resp, err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), token) {
				t.Fatalf("status/body = %d/%q, want 401 without token", resp.StatusCode, body)
			}
		})
	}
	if strings.Contains(logBuffer.String(), token) {
		t.Fatalf("logs contain plaintext token: %s", logBuffer.String())
	}
}

func TestServeWSSubprotocolOnlyOnWSPath(t *testing.T) {
	endpoint, token, _, _ := newServeWSFixture(t, 2)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.Replace(endpoint, "ws://", "http://", 1)+"/api/v1/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A real upgrade request: only the path decides whether the credential is honoured.
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Protocol", "rush.v1, rush.bearer."+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("non-WS subprotocol status = %d, want 401", resp.StatusCode)
	}
}

func TestServeWSProductionOriginAndUIHelloIntegration(t *testing.T) {
	allowedOrigin := "http://allowed.example:8080"
	endpoint, token, _, _ := newServeWSFixtureWithOrigins(t, 2, []string{allowedOrigin})
	originEndpoint, originToken := endpoint, token
	originCases := []struct {
		name   string
		origin string
		status int
	}{
		{name: "exact allowed", origin: allowedOrigin, status: http.StatusSwitchingProtocols},
		{name: "different port", origin: "http://allowed.example:8081", status: http.StatusForbidden},
		{name: "different scheme", origin: "https://allowed.example:8080", status: http.StatusForbidden},
		{name: "null", origin: "null", status: http.StatusForbidden},
		{name: "missing origin", origin: "", status: http.StatusSwitchingProtocols},
		{name: "unlisted loopback", origin: "http://localhost:8080", status: http.StatusForbidden},
	}
	for _, tc := range originCases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"Authorization": []string{"Bearer " + originToken}, "Origin": []string{tc.origin}}
			conn, response, err := (&websocket.Dialer{HandshakeTimeout: 3 * time.Second}).Dial(originEndpoint+"/ws", h) //nolint:bodyclose // gorilla upgrade responses carry no body
			if tc.status == http.StatusSwitchingProtocols {
				if err != nil {
					t.Fatalf("allowed origin rejected: %v (%v)", err, response)
				}
				conn.Close()
				return
			}
			if err == nil || conn != nil || response == nil || response.StatusCode != tc.status {
				t.Fatalf("origin %q response=%v err=%v, want %d", tc.origin, response, err, tc.status)
			}
		})
	}
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	header := http.Header{"Authorization": []string{"Bearer " + token}, "Origin": []string{"http://localhost:8080"}}
	c, response, err := d.Dial(endpoint+"/ws", header) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err == nil || c != nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("unconfigured browser origin accepted: conn=%v response=%v err=%v", c, response, err)
	}

	ui := New(nil, "127.0.0.1:0", nil)
	uiEndpoint := startPolicyServer(t, ui)
	d = websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	uiURL, err := url.Parse(uiEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	uiHeader := http.Header{"Authorization": []string{"Bearer " + ui.Token()}}
	uiConn, response, err := d.Dial("ws"+strings.TrimPrefix(uiEndpoint, "http")+"/ws", uiHeader) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("UI websocket with localhost origin failed: %v (%v)", err, response)
	}
	_ = uiConn.Close()
	evilHeader := http.Header{"Authorization": []string{"Bearer " + ui.Token()}, "Origin": []string{"http://evil.example"}}
	if evilConn, evilResponse, evilErr := d.Dial("ws"+strings.TrimPrefix(uiEndpoint, "http")+"/ws", evilHeader); evilErr == nil || evilConn != nil || evilResponse == nil || evilResponse.StatusCode != http.StatusForbidden { //nolint:bodyclose // gorilla upgrade responses carry no body
		t.Fatalf("UI accepted evil origin: conn=%v response=%v err=%v", evilConn, evilResponse, evilErr)
	}
	uiHeader = http.Header{"Authorization": []string{"Bearer " + ui.Token()}, "Origin": []string{"http://localhost:" + uiURL.Port()}}
	uiConn, response, err = d.Dial("ws"+strings.TrimPrefix(uiEndpoint, "http")+"/ws", uiHeader) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("UI websocket failed: %v (%v)", err, response)
	}
	defer uiConn.Close()
	command, err := json.Marshal(WSMessage{ID: "ui-command", Type: "not_a_command"})
	if err != nil {
		t.Fatal(err)
	}
	if err := uiConn.WriteMessage(websocket.TextMessage, command); err != nil {
		t.Fatal(err)
	}
	_ = uiConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, frame, err := uiConn.ReadMessage()
	if err != nil {
		t.Fatalf("read first UI frame: %v", err)
	}
	var message WSMessage
	if err := json.Unmarshal(frame, &message); err != nil {
		t.Fatalf("decode first UI frame %q: %v", frame, err)
	}
	if message.Type == EventHello {
		t.Fatalf("first UI frame is hello: %s", frame)
	}
}

func equalHeaders(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for key, values := range a {
		other, ok := b[key]
		if !ok || strings.Join(values, "\x00") != strings.Join(other, "\x00") {
			return false
		}
	}
	return true
}
