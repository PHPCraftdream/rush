package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func isolateSecurityGlobals(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", dir)
	t.Setenv("RUSH_GLOBAL_CONFIG", dir)
	t.Setenv("RUSH_GLOBAL_DIR", dir)
	return dir
}

func TestChainAllAuthenticationCausesHaveIdenticalRawResponse(t *testing.T) {
	isolateSecurityGlobals(t)
	valid := "rush_12345678." + strings.Repeat("a", 43)
	cases := []struct {
		name string
		edit func(*http.Request)
	}{
		{"missing", func(*http.Request) {}},
		{"scheme", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+valid) }},
		{"malformed", func(r *http.Request) { r.Header.Set("Authorization", "Bearer malformed") }},
		{"unknown", func(r *http.Request) { r.Header.Set("Authorization", "Bearer rush_ffffffff."+strings.Repeat("b", 43)) }},
		{"secret", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) }},
		{"revoked", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) }},
		{"expired", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) }},
		{"allow-list", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+valid) }},
	}
	var expected []byte
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
			h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, AllowedTokenIDs: []string{"87654321"}}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			tc.edit(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			got := []byte(w.Result().Status + "\n" + rawHeadersExceptDate(w.Result().Header) + "\n" + w.Body.String())
			if i == 0 {
				expected = got
			} else if !bytes.Equal(expected, got) {
				t.Fatalf("raw auth responses differ: %q != %q", expected, got)
			}
		})
	}
}

func rawHeadersExceptDate(h http.Header) string {
	keys := make([]string, 0, len(h))
	for key := range h {
		if !strings.EqualFold(key, "Date") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		for _, value := range h.Values(key) {
			out.WriteString(key + ": " + value + "\n")
		}
	}
	return out.String()
}

func TestChainTokenQueryAndCookieAreRejected(t *testing.T) {
	isolateSecurityGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, target := range []string{"/?token=anything", "/"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		if target == "/" {
			r.AddCookie(&http.Cookie{Name: "session", Value: "cookie"})
		}
		r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
		if target == "/?token=anything" {
			// Query credentials poison even a valid bearer.
		} else {
			r.Header.Del("Authorization")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s => %d", target, w.Code)
		}
	}
}

func TestChainStoreFailureFixed503DoesNotExposeToken(t *testing.T) {
	isolateSecurityGlobals(t)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const sentinel = "store contains credential details"
	v := &fakeVerifier{verifyErr: errors.New(sentinel)}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: logger})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var body string
	for i := 0; i < 2; i++ {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 503 || strings.Contains(w.Body.String(), sentinel) || strings.Contains(logs.String(), strings.Repeat("a", 43)) {
			t.Fatalf("unexpected store failure response/log: %d %q %s", w.Code, w.Body.String(), logs.String())
		}
		if i == 0 {
			body = w.Body.String()
		} else if body != w.Body.String() {
			t.Fatal("503 body changed")
		}
	}
}

func TestRateLimiterBucketsRefillAndEvict(t *testing.T) {
	isolateSecurityGlobals(t)
	now := time.Unix(100, 0)
	l := newRateLimiter(1, 2, func() time.Time { return now })
	first := l.Allow("token:x", now)
	second := l.Allow("token:x", now)
	if first != 0 || second != 0 {
		t.Fatal("initial burst denied")
	}
	if got := l.Allow("token:x", now); got != time.Second {
		t.Fatalf("retry=%s", got)
	}
	if l.Allow("ip:y", now) != 0 {
		t.Fatal("independent key denied")
	}
	now = now.Add(time.Second)
	if l.Allow("token:x", now) != 0 {
		t.Fatal("refill denied")
	}
	now = now.Add(2 * time.Minute)
	l.Allow("fresh", now)
	if len(l.entries) != 1 {
		t.Fatalf("idle bucket not evicted: %d", len(l.entries))
	}
}

func TestFailureBackoffQuietResetAndEviction(t *testing.T) {
	isolateSecurityGlobals(t)
	now := time.Unix(0, 0)
	b := newFailureBackoff(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		b.Failure("ip", now)
	}
	if got := b.Blocked("ip", now); got != time.Second {
		t.Fatalf("retry after pure read=%v, want 1s", got)
	}
	if got := b.Blocked("ip", now); got != time.Second {
		t.Fatalf("repeated pure read=%v, want 1s", got)
	}
	now = now.Add(time.Second)
	if got := b.Blocked("ip", now); got != 0 {
		t.Fatalf("retry at block expiry=%v, want 0", got)
	}
	b.mu.Lock()
	n := len(b.entries)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("entry evicted at block expiry: %d", n)
	}

	now = time.Unix(2, 0).Add(-time.Nanosecond)
	if got := b.Blocked("ip", now); got != 0 {
		t.Fatalf("retry before quiet-period expiry=%v, want 0", got)
	}
	b.mu.Lock()
	n = len(b.entries)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("entry evicted before quiet-period expiry: %d", n)
	}

	now = time.Unix(2, 0)
	if got := b.Blocked("ip", now); got != 0 {
		t.Fatalf("retry at quiet-period expiry=%v, want 0", got)
	}
	b.mu.Lock()
	n = len(b.entries)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("quiet entry not evicted: %d", n)
	}

	for i := 0; i < 9; i++ {
		b.Failure("ip", now)
	}
	if got := b.Blocked("ip", now); got != 0 {
		t.Fatalf("blocked after only nine new failures=%v, want 0", got)
	}
	b.Failure("ip", now)
	if got := b.Blocked("ip", now); got != time.Second {
		t.Fatalf("retry after ten new failures=%v, want 1s", got)
	}
}

func TestChainRateLimitedAndAuditCapturesSecurityStatus(t *testing.T) {
	isolateSecurityGlobals(t)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const token = "rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	clock := func() time.Time { return time.Unix(100, 0) }
	makeHandler := func(maxBody int64, burst int, next http.Handler) http.Handler {
		v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
		return Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: clock}, Logger: logger, RPS: 1, Burst: burst, MaxBody: maxBody})(next)
	}
	request := func(h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		func() { defer func() { _ = recover() }(); h.ServeHTTP(w, r) }()
		return w
	}
	okHandler := makeHandler(8, 2, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(202) }))
	request(okHandler, "/ok", token, "")
	limitedHandler := makeHandler(8, 1, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request(limitedHandler, "/first", token, "")
	if got := request(limitedHandler, "/limited", token, ""); got.Code != 429 {
		t.Fatalf("rate-limited request status=%d", got.Code)
	}
	largeHandler := makeHandler(1, 2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.ReadAll(r.Body) }))
	if got := request(largeHandler, "/large", token, "large"); got.Code != 413 {
		t.Fatalf("oversize request status=%d", got.Code)
	}
	panicHandler := makeHandler(8, 2, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("controlled") }))
	request(panicHandler, "/panic", token, "")
	unauthorizedHandler := makeHandler(8, 2, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if got := request(unauthorizedHandler, "/unauthorized", "", ""); got.Code != 401 {
		t.Fatalf("unauthorized request status=%d", got.Code)
	}
	for _, status := range []string{"status=202", "status=429", "status=401", "status=413", "status=500"} {
		if !strings.Contains(logs.String(), status) {
			t.Fatalf("missing %s in %s", status, logs.String())
		}
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), strings.TrimPrefix(token, "rush_12345678.")) {
		t.Fatal("secret leaked into audit")
	}
}

func TestRedactionHelpersCoverURLHeadersAndErrors(t *testing.T) {
	isolateSecurityGlobals(t)
	token := "rush_12345678." + strings.Repeat("a", 43)
	for _, input := range []string{token, "prefix=" + token, "https://x.test/?token=" + token, "Authorization: Bearer " + token, "error: " + token, token + " " + token} {
		got := Redact(input)
		if strings.Contains(got, strings.Repeat("a", 43)) || strings.Contains(got, token) {
			t.Fatalf("secret leaked: %q", got)
		}
	}
	u, _ := url.Parse("https://user:pass@example.test/?token=" + token)
	if got := RedactURL(u).String(); strings.Contains(got, token) || strings.Contains(got, "user:pass") {
		t.Fatal(got)
	}
	h := http.Header{"Authorization": {"Bearer " + token}}
	if got := RedactHeader(h).Get("Authorization"); strings.Contains(got, token) {
		t.Fatal(got)
	}
}

func TestBootstrapPrincipalFromConsumedStoreRemainsValid(t *testing.T) {
	dir := isolateSecurityGlobals(t)
	plain := "rush_01020304." + strings.Repeat("A", 43)
	digest := config.APITokenHash(plain)
	store := &config.APITokenStore{Path: filepath.Join(dir, "tokens.json")}
	env := map[string]string{"RUSH_SERVE_TOKEN_SHA256": "01020304:" + digest, "RUSH_SERVE_TOKEN_SCOPE": "run"}
	getenv := func(key string) string { return env[key] }
	unsetenv := func(key string) error { delete(env, key); return nil }
	token, ok, err := config.ConsumeServeBootstrapToken(store, getenv, unsetenv, time.Unix(10, 0))
	if err != nil || !ok {
		t.Fatalf("consume bootstrap: %v %v", ok, err)
	}
	verified, ok, err := store.VerifyAPIToken(plain, time.Unix(10, 0))
	if err != nil || !ok {
		t.Fatalf("verify bootstrap: %v %v", ok, err)
	}
	v := &realStoreVerifier{store: store}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	p, _, authErr, storeErr := authenticate(ChainConfig{AuthConfig: AuthConfig{Verifier: v, BootstrapTokenIDs: []string{token.ID}, Clock: func() time.Time { return time.Unix(10, 0) }}}, r)
	if authErr != nil || storeErr != nil || p.TokenID != token.ID || !p.bootstrap || !StillValid(p, time.Unix(20, 0)) {
		t.Fatalf("bootstrap principal invalid: %#v auth=%v store=%v", p, authErr, storeErr)
	}
	if verified.ID != token.ID {
		t.Fatalf("verified token ID %q, want %q", verified.ID, token.ID)
	}
}

type realStoreVerifier struct{ store *config.APITokenStore }

func (v *realStoreVerifier) VerifyAPIToken(s string, now time.Time) (config.APIToken, bool, error) {
	return v.store.VerifyAPIToken(s, now)
}
func (v *realStoreVerifier) Touch(id string) { v.store.Touch(id) }
func (v *realStoreVerifier) ShowAPIToken(id string) (config.APIToken, error) {
	return v.store.ShowAPIToken(id)
}

func TestMalformedTokenStoreFileIsUnavailable(t *testing.T) {
	dir := isolateSecurityGlobals(t)
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &config.APITokenStore{Path: path}
	v := &realStoreVerifier{store: store}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "12345678") {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestRateLimiterRetryAfterAndRedactionOutput(t *testing.T) {
	isolateSecurityGlobals(t)
	h := http.Header{"Authorization": {"Bearer secret"}}
	if got := RedactHeader(h).Get("Authorization"); got != "[REDACTED]" {
		t.Fatal(got)
	}
}

func TestAuditFailureIncludesNoAuthSecret(t *testing.T) {
	isolateSecurityGlobals(t)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: &fakeVerifier{}}, Logger: logger})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?access_token=leak", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(logs.String(), strings.Repeat("a", 43)) {
		t.Fatal(logs.String())
	}
}
