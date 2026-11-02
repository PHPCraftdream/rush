package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestRealAPITokenStoreInvalidCredentialsHaveIdentical401(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	now := time.Unix(1_800_000_000, 0)
	valid, validRecord, err := store.CreateAPIToken("valid", []string{"read"}, "ask", nil)
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
	past := now.Add(-time.Second)
	expired, _, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &past)
	if err != nil {
		t.Fatal(err)
	}
	wrongSecret := []byte(valid)
	secretOffset := strings.IndexByte(valid, '.') + 1
	if wrongSecret[secretOffset] == 'A' {
		wrongSecret[secretOffset] = 'B'
	} else {
		wrongSecret[secretOffset] = 'A'
	}
	cases := []struct {
		name  string
		token string
	}{
		{name: "unknown id", token: "rush_ffffffff." + strings.Repeat("A", 43)},
		{name: "wrong secret", token: string(wrongSecret)},
		{name: "revoked", token: revoked},
		{name: "expired", token: expired},
	}
	handler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store, Clock: func() time.Time { return now }}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := func(token string) string {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result().Status + "\n" + rawHeadersExceptDate(w.Result().Header) + "\n" + w.Body.String()
	}
	missing := request("")
	if !strings.HasPrefix(missing, "401 Unauthorized\n") {
		t.Fatalf("missing-header response = %q", missing)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := request(tc.token); got != missing {
				t.Fatalf("response differs from missing-header 401:\n got %q\nwant %q", got, missing)
			}
		})
	}
	if validRecord.ID == "" {
		t.Fatal("CreateAPIToken returned an empty id")
	}
}

func TestRealAPITokenStoreMalformedFileReturnsFixed503(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	path := store.Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("A", 43))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	got := w.Result().Status + "\n" + rawHeadersExceptDate(w.Result().Header) + "\n" + w.Body.String()
	want := "503 Service Unavailable\nContent-Type: application/problem+json\n\n{\"type\":\"about:blank\",\"title\":\"Service Unavailable\",\"status\":503}\n"
	if got != want {
		t.Fatalf("response = %q, want %q (global path %s)", got, want, path)
	}
}
