package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type fakeVerifier struct {
	token     config.APIToken
	verifyErr error
	ok        bool
	touches   int
}

func (f *fakeVerifier) VerifyAPIToken(string, time.Time) (config.APIToken, bool, error) {
	return f.token, f.ok, f.verifyErr
}
func (f *fakeVerifier) Touch(string)                                 { f.touches++ }
func (f *fakeVerifier) ShowAPIToken(string) (config.APIToken, error) { return f.token, nil }

func isolateAPIGlobals(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logTempTreeBeforeRemoval(t, dir)
	t.Setenv("RUSH_GLOBAL_DATA", dir)
	t.Setenv("RUSH_GLOBAL_CONFIG", dir)
	t.Setenv("RUSH_GLOBAL_DIR", dir)
}

func TestChainUniformAuthenticationFailures(t *testing.T) {
	isolateAPIGlobals(t)
	cases := []func(*http.Request){func(*http.Request) {}, func(r *http.Request) { r.Header.Set("Authorization", "Basic abc") }, func(r *http.Request) { r.Header.Set("Authorization", "Bearer invalid") }, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	}}
	var want []byte
	for i, change := range cases {
		v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
		h := Chain(ChainConfig{Verifier: v, Clock: func() time.Time { return time.Unix(1, 0) }})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
		change(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := []byte(fmt.Sprintf("%d|%v|%s", w.Code, w.Header(), w.Body.String()))
		if i == 0 {
			want = got
		} else if !bytes.Equal(want, got) {
			t.Fatalf("failure responses differ: %q != %q", want, got)
		}
	}
}

func TestChainOrderAuditsAndRejects(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678", Scopes: []string{"run"}}, ok: true}
	reached := false
	h := Chain(ChainConfig{Verifier: v})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || reached {
		t.Fatal("unauthenticated request reached handler")
	}
}

func TestChainConfigValidate(t *testing.T) {
	isolateAPIGlobals(t)
	if (ChainConfig{}).Validate() == nil {
		t.Fatal("empty verifier accepted")
	}
	if (ChainConfig{Verifier: &fakeVerifier{}, RPS: -1}).Validate() == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestRedactSecrets(t *testing.T) {
	isolateAPIGlobals(t)
	token := "rush_12345678." + strings.Repeat("a", 43)
	if strings.Contains(Redact("err "+token), strings.Repeat("a", 43)) {
		t.Fatal("secret not redacted")
	}
	if strings.Contains(Redact("Authorization: Bearer "+token), token) {
		t.Fatal("header not redacted")
	}
}

func TestStillValid(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Now()
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}}
	p := Principal{TokenID: "12345678", verifier: v}
	if !StillValid(p, now) {
		t.Fatal("valid token rejected")
	}
	v.token.RevokedAt = &now
	if StillValid(p, now) {
		t.Fatal("revoked token accepted")
	}
}

func TestStillValidMissingTokenOnlyAllowsBootstrap(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(100, 0)
	v := &missingVerifier{}
	ordinary := Principal{TokenID: "12345678", verifier: v}
	bootstrap := Principal{TokenID: "12345678", verifier: v, bootstrap: true}
	expires := now.Add(time.Minute)
	if StillValid(ordinary, now) {
		t.Fatal("removed ordinary token remained valid")
	}
	if !StillValid(bootstrap, now) {
		t.Fatal("unexpired bootstrap token rejected")
	}
	bootstrap.expires = &expires
	if StillValid(bootstrap, expires) {
		t.Fatal("expired bootstrap token accepted")
	}
}

func TestRemovedOrdinaryTokenIsNotBootstrap(t *testing.T) {
	isolateAPIGlobals(t)
	v := &missingVerifier{fakeVerifier: fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	p, _, authErr, storeErr := authenticate(ChainConfig{AuthConfig: AuthConfig{Verifier: v, AllowedTokenIDs: []string{"12345678"}, Clock: func() time.Time { return time.Unix(1, 0) }}}, r)
	if authErr == nil || storeErr != nil || p.TokenID != "" {
		t.Fatalf("removed ordinary token accepted: principal=%#v auth=%v store=%v", p, authErr, storeErr)
	}
}

func TestAuthenticateZeroClockDoesNotPanic(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	_, _, _, _ = authenticate(ChainConfig{AuthConfig: AuthConfig{Verifier: v}}, r)
}

type missingVerifier struct{ fakeVerifier }

func (v *missingVerifier) ShowAPIToken(string) (config.APIToken, error) {
	return config.APIToken{}, config.ErrAPITokenNotFound
}

func TestChainOversizeBodyReturnsFixed413(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	const token = "rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	called := false
	h := Chain(ChainConfig{Verifier: v, MaxBody: 3})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("overflow read unexpectedly succeeded")
		}
	}))
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader("four"))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !called || w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != tooLargeBody {
		t.Fatalf("called=%v status=%d body=%q", called, w.Code, w.Body.String())
	}
}

func TestChainOverflowCannotRewriteStartedResponse(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	h := Chain(ChainConfig{Verifier: v, MaxBody: 3})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("overflow read unexpectedly succeeded")
		}
	}))
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader("four"))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("started response rewritten: status=%d", w.Code)
	}
}

func TestUnknownLengthBodyStreamsAndPreservesClose(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	body := &trackingReadCloser{Reader: strings.NewReader("payload")}
	seen := ""
	h := Chain(ChainConfig{Verifier: v, MaxBody: 20})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1)
		if _, err := r.Body.Read(buf); err != nil {
			t.Errorf("first streaming read: %v", err)
		}
		seen = string(buf)
		if err := r.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	r.Body = body
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "p" || !body.closed {
		t.Fatalf("first byte=%q original body closed=%v", seen, body.closed)
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (b *trackingReadCloser) Close() error {
	b.closed = true
	return nil
}

func TestFailureBackoffDeterministicWindowAndRetry(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(0, 0)
	b := newFailureBackoff(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		b.Failure("192.0.2.4", now)
	}
	b.mu.Lock()
	initial := b.entries["192.0.2.4"].blockedUntil.Sub(now)
	b.mu.Unlock()
	if initial != time.Second {
		t.Fatalf("initial block=%v", initial)
	}
	if got := b.Blocked("192.0.2.4", now); got != time.Second {
		t.Fatalf("blocked request changed retry=%v, want 1s", got)
	}
}

func TestBackoffIgnoresForwardedIP(t *testing.T) {
	isolateAPIGlobals(t)
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "192.0.2.4:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := sourceIP(r.RemoteAddr); got != "192.0.2.4" {
		t.Fatal(got)
	}
}
