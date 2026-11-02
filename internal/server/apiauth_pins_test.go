package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestChainListenerPinsRejectUnpinnedAndCheckWorkspace(t *testing.T) {
	isolateAPIGlobals(t)
	workspace := t.TempDir()
	v := &fakeVerifier{token: config.APIToken{ID: "12345678", Scopes: []string{"run"}, Permission: "ask"}, ok: true}
	pins := map[string]string{"12345678": "session"}
	handler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Pins: pins, PinsDeclared: true, Workspace: workspace, Clock: func() time.Time { return time.Unix(1, 0) }}})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := req().Code; got != http.StatusNoContent {
		t.Fatalf("pinned token response=%d", got)
	}
	v.token.ID = "87654321"
	if got := req().Code; got != http.StatusUnauthorized {
		t.Fatalf("unpinned token response=%d", got)
	}
}

func TestChainPinnedTokenWorkspaceMismatchIsForbidden(t *testing.T) {
	isolateAPIGlobals(t)
	workspace := t.TempDir()
	pinWorkspace := t.TempDir()
	v := &fakeVerifier{token: config.APIToken{ID: "12345678", Scopes: []string{"run"}, Permission: "ask", Binding: config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "s", Workspace: pinWorkspace}}}, ok: true}
	handler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Workspace: workspace, Clock: func() time.Time { return time.Unix(1, 0) }}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "token-not-for-this-workspace") {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}
