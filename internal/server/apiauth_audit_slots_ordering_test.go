package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

const finishCredential = "rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type finishVerifier struct {
	mu        sync.Mutex
	verifyErr error
	ok        bool
	calls     int
}

func (v *finishVerifier) VerifyAPIToken(raw string, _ time.Time) (config.APIToken, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.verifyErr != nil {
		return config.APIToken{}, false, v.verifyErr
	}
	return config.APIToken{ID: raw[5:13], Scopes: []string{"run"}}, v.ok, nil
}
func (v *finishVerifier) Touch(string) {}
func (v *finishVerifier) ShowAPIToken(id string) (config.APIToken, error) {
	if !v.ok || v.verifyErr != nil {
		return config.APIToken{}, config.ErrAPITokenNotFound
	}
	return config.APIToken{ID: id, Scopes: []string{"run"}}, nil
}
func (v *finishVerifier) callCount() int { v.mu.Lock(); defer v.mu.Unlock(); return v.calls }

func TestBackoffEscalatesAtExpiryAndResetsAfterQuietPeriod(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(1000, 0)
	v := &finishVerifier{}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, RPS: 1000, Burst: 1000})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.77:1234"
		r.Header.Set("Authorization", "Bearer "+finishCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 10; i++ {
		if got := request(); got.Code != 401 {
			t.Fatalf("failure %d status=%d", i, got.Code)
		}
	}
	if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != "1" {
		t.Fatalf("first block=%d retry=%q", got.Code, got.Header().Get("Retry-After"))
	}
	for _, step := range []struct {
		delay time.Duration
		retry string
	}{{time.Second, "2"}, {2 * time.Second, "4"}, {4 * time.Second, "8"}} {
		now = now.Add(step.delay)
		before := v.callCount()
		if got := request(); got.Code != 401 || v.callCount() != before+1 {
			t.Fatalf("expiry request status=%d verifier delta=%d", got.Code, v.callCount()-before)
		}
		if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != step.retry {
			t.Fatalf("escalated block=%d retry=%q want=%s", got.Code, got.Header().Get("Retry-After"), step.retry)
		}
	}
	for offence := 4; offence <= 12; offence++ {
		now = now.Add(backoffDelay(offence))
		if got := request(); got.Code != 401 {
			t.Fatalf("cap step %d status=%d", offence, got.Code)
		}
		want := retryAfterSeconds(backoffDelay(offence + 1))
		if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != want {
			t.Fatalf("cap step %d status=%d retry=%q want=%q", offence, got.Code, got.Header().Get("Retry-After"), want)
		}
	}
}

func TestAuditExactFieldsAndRouteTemplate(t *testing.T) {
	isolateAPIGlobals(t)
	capture := &captureHandler{notify: make(chan struct{}, 4)}
	v := &finishVerifier{ok: true}
	mux := http.NewServeMux()
	chain := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: slog.New(capture), RPS: 1000, Burst: 1000})
	mux.Handle("GET /things/{id}", chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		RecordSession(r.Context(), "session-1")
		_, _ = w.Write([]byte("yes"))
	})))
	unknown := chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeUnauthorized(w) }))
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/things/42" {
			mux.ServeHTTP(w, r)
			return
		}
		unknown.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(dispatch)
	defer srv.Close()
	client := srv.Client()
	for _, tc := range []struct {
		path string
		auth bool
	}{{"/things/42", true}, {"/unknown", false}} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+finishCredential)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	records := waitForRecords(t, capture, 2)
	wantKeys := []string{"bytes", "certificate_fingerprint", "duration", "method", "remote_addr", "route", "scope", "session_id", "status", "token_id"}
	for i, rec := range records[:2] {
		keys := make([]string, 0, len(rec.attrs))
		for k := range rec.attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("record keys=%v want=%v", keys, wantKeys)
		}
		remote, _, err := net.SplitHostPort(fmt.Sprint(rec.attrs["remote_addr"]))
		if err != nil || net.ParseIP(remote) == nil {
			t.Fatalf("remote address is not a TCP peer: %v", rec.attrs["remote_addr"])
		}
		text := rec.message + " " + fmt.Sprint(rec.attrs)
		if strings.Contains(text, finishCredential) || strings.Contains(text, strings.Repeat("a", 43)) {
			t.Fatalf("audit leaked plaintext token: %s", text)
		}
		if i == 0 {
			for k, want := range map[string]any{"route": "GET /things/{id}", "scope": "run", "token_id": "12345678", "session_id": "session-1", "status": 200, "bytes": 3} {
				if rec.attrs[k] != want {
					t.Fatalf("success %s=%v want=%v", k, rec.attrs[k], want)
				}
			}
		} else {
			for k, want := range map[string]any{"route": "unmatched", "scope": "none", "token_id": "", "status": 401, "bytes": len(authFailureBody)} {
				if rec.attrs[k] != want {
					t.Fatalf("failure %s=%v want=%v", k, rec.attrs[k], want)
				}
			}
		}
	}
}

func TestStoreFailuresDoNotCountTowardBackoff(t *testing.T) {
	isolateAPIGlobals(t)
	v := &finishVerifier{verifyErr: errors.New("unreadable")}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, RPS: 1000, Burst: 1000})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.88:9"
		r.Header.Set("Authorization", "Bearer "+finishCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 15; i++ {
		if w := request(); w.Code != 503 || w.Body.String() != authUnavailableBody {
			t.Fatalf("store error status=%d body=%q", w.Code, w.Body.String())
		}
	}
	v.mu.Lock()
	v.verifyErr = nil
	v.ok = true
	v.mu.Unlock()
	if w := request(); w.Code != 200 {
		t.Fatalf("working store status=%d", w.Code)
	}
}

func TestChainConcurrentSlotsAndPanicRelease(t *testing.T) {
	isolateAPIGlobals(t)
	v := &finishVerifier{ok: true}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, MaxConnsPerToken: 2, RPS: 1000, Burst: 1000})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("controlled")
		}
		if r.URL.Path == "/quick" {
			w.WriteHeader(http.StatusOK)
			return
		}
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = recover() }()
		h.ServeHTTP(w, r)
	}))
	client := srv.Client()
	client.Timeout = 10 * time.Second
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		srv.Close()
	})
	do := func(path, id string) (int, string, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "Bearer rush_"+id+"."+strings.Repeat("a", 43))
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Retry-After"), nil
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			code, _, err := do("/hold", "12345678")
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("held status=%d", code)
			}
			results <- err
		}()
	}
	waitEntered := func(count int) {
		for i := 0; i < count; i++ {
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for held handler")
			}
		}
	}
	waitEntered(2)
	if code, retry, err := do("/hold", "12345678"); err != nil || code != 429 || retry != "1" {
		t.Fatalf("third same-token request status=%d retry=%q err=%v", code, retry, err)
	}
	if code, _, err := do("/quick", "87654321"); err != nil || code != 200 {
		t.Fatalf("other-token request status=%d err=%v", code, err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("held request did not finish")
		}
	}
	if code, _, err := do("/quick", "12345678"); err != nil || code != 200 {
		t.Fatalf("same-token request after release status=%d err=%v", code, err)
	}
	if code, _, err := do("/panic", "12345678"); err != nil || code == 0 {
		t.Fatalf("panic request status=%d err=%v", code, err)
	}
	release = make(chan struct{})
	panicHeld := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			code, _, err := do("/hold", "12345678")
			if err == nil && code != http.StatusOK {
				err = fmt.Errorf("post-panic held status=%d", code)
			}
			panicHeld <- err
		}()
	}
	waitEntered(2)
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-panicHeld:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("post-panic held request did not finish")
		}
	}
}

func TestChainOrderingQueryRejectionAndAudit(t *testing.T) {
	isolateAPIGlobals(t)
	capture := &captureHandler{notify: make(chan struct{}, 16)}
	v := &finishVerifier{ok: false}
	reached := 0
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: slog.New(capture), RPS: 1000, Burst: 1000})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	calls := v.callCount()
	for _, q := range []string{"token", "access_token"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?"+q+"="+finishCredential, nil)
		r.RemoteAddr = "192.0.2.22:1"
		r.Header.Set("Authorization", "Bearer "+finishCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("query %s status=%d", q, w.Code)
		}
	}
	if v.callCount() != calls {
		t.Fatalf("query credentials called verifier %d times", v.callCount()-calls)
	}
	for i := 0; i < 10; i++ {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.23:1"
		r.Header.Set("Authorization", "Bearer "+finishCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("backoff setup failure %d status=%d", i, w.Code)
		}
	}
	calls = v.callCount()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.23:1"
	r.Header.Set("Authorization", "Bearer "+finishCredential)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || v.callCount() != calls {
		t.Fatalf("backoff status=%d verifier delta=%d", w.Code, v.callCount()-calls)
	}
	if reached != 0 {
		t.Fatalf("unauthenticated requests reached handler %d times", reached)
	}
	records := waitForRecords(t, capture, 13)
	audited := false
	for _, rec := range records {
		if rec.attrs["status"] == 401 {
			audited = true
			break
		}
	}
	if !audited {
		t.Fatal("401 failure was not audited")
	}
}

func TestBackoffEscalatesAndRequiresQuietPeriod(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(20, 0)
	v := &finishVerifier{}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, RPS: 1000, Burst: 1000})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.33:1"
		r.Header.Set("Authorization", "Bearer "+finishCredential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 10; i++ {
		if got := request(); got.Code != 401 {
			t.Fatalf("initial failure %d status=%d", i, got.Code)
		}
	}
	if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != "1" {
		t.Fatalf("first block status=%d retry=%q", got.Code, got.Header().Get("Retry-After"))
	}
	delays := []struct {
		block time.Duration
		retry string
	}{{time.Second, "2"}, {2 * time.Second, "4"}, {4 * time.Second, "8"}, {8 * time.Second, "16"}, {16 * time.Second, "32"}, {32 * time.Second, "64"}, {64 * time.Second, "128"}, {128 * time.Second, "256"}, {256 * time.Second, "300"}, {300 * time.Second, "300"}}
	for i, step := range delays {
		now = now.Add(step.block)
		before := v.callCount()
		if got := request(); got.Code != 401 || v.callCount() != before+1 {
			t.Fatalf("expiry %d status=%d verifier delta=%d", i, got.Code, v.callCount()-before)
		}
		if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != step.retry {
			t.Fatalf("escalated block %d status=%d retry=%q want=%s", i, got.Code, got.Header().Get("Retry-After"), step.retry)
		}
	}
	v.mu.Lock()
	v.ok = true
	v.mu.Unlock()
	now = now.Add(300 * time.Second)
	if got := request(); got.Code != 200 {
		t.Fatalf("success after expiry status=%d", got.Code)
	}
	v.mu.Lock()
	v.ok = false
	v.mu.Unlock()
	if got := request(); got.Code != 401 {
		t.Fatalf("failure after success status=%d", got.Code)
	}
	if got := request(); got.Code != 429 || got.Header().Get("Retry-After") != "300" {
		t.Fatalf("block after success status=%d retry=%q", got.Code, got.Header().Get("Retry-After"))
	}
}
