package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type recordingVerifier struct {
	mu      sync.Mutex
	token   config.APIToken
	ok      bool
	err     error
	touches []string
	flushes int
}

func (v *recordingVerifier) VerifyAPIToken(string, time.Time) (config.APIToken, bool, error) {
	return v.token, v.ok, v.err
}

func (v *recordingVerifier) Touch(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.touches = append(v.touches, id)
}

func (v *recordingVerifier) ShowAPIToken(id string) (config.APIToken, error) {
	if v.token.ID != id {
		return config.APIToken{}, config.ErrAPITokenNotFound
	}
	return v.token, nil
}

func (v *recordingVerifier) Flush() error { v.mu.Lock(); defer v.mu.Unlock(); v.flushes++; return nil }

func TestFailureBackoffQuietPeriodStartsAtLastFailure(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(100, 0)
	b := newFailureBackoff(func() time.Time { return now })
	for i := 0; i < 9; i++ {
		b.Failure("peer", now)
	}
	if got := b.Blocked("peer", now); got != 0 {
		t.Fatalf("nine failures retry=%s want=0", got)
	}
	b.Failure("peer", now)
	if got := b.Blocked("peer", now); got != time.Second {
		t.Fatalf("tenth failure retry=%s want=1s", got)
	}
	blockedUntil := now.Add(time.Second)
	now = blockedUntil
	if got := b.Blocked("peer", now); got != 0 {
		t.Fatalf("retry at block expiry=%s want=0", got)
	}
	b.Failure("peer", now)
	if got := b.Blocked("peer", now); got != 2*time.Second {
		t.Fatalf("retry after expiry failure=%s want=2s", got)
	}
	quietEnd := now.Add(4 * time.Second)
	now = quietEnd.Add(-time.Nanosecond)
	b.Failure("peer", now)
	if got := b.Blocked("peer", now); got != 4*time.Second {
		t.Fatalf("failure before quiet boundary retry=%s want=4s", got)
	}
	now = now.Add(8 * time.Second)
	for i := 0; i < 9; i++ {
		b.Failure("peer", now)
	}
	if got := b.Blocked("peer", now); got != 0 {
		t.Fatalf("nine failures after quiet boundary retry=%s want=0", got)
	}
	b.Failure("peer", now)
	if got := b.Blocked("peer", now); got != time.Second {
		t.Fatalf("tenth failure after quiet boundary retry=%s want=1s", got)
	}
}

func TestChainTouchesOnlySuccessfulAuthentication(t *testing.T) {
	isolateAPIGlobals(t)
	const token = "rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name    string
		header  string
		ok      bool
		err     error
		allowed []string
	}{
		{"missing", "", false, nil, nil},
		{"scheme", "Basic " + token, false, nil, nil},
		{"malformed", "Bearer malformed", false, nil, nil},
		{"invalid", "Bearer " + token, false, nil, nil},
		{"revoked", "Bearer " + token, false, config.ErrInvalidAPIToken, nil},
		{"store", "Bearer " + token, false, errors.New("store"), nil},
		{"allow-list", "Bearer " + token, true, nil, []string{"87654321"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: tc.ok, err: tc.err}
			h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, AllowedTokenIDs: tc.allowed}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = "192.0.2.1:1"
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			v.mu.Lock()
			defer v.mu.Unlock()
			if len(v.touches) != 0 {
				t.Fatalf("failure touched %v", v.touches)
			}
			if v.flushes != 0 {
				t.Fatal("request flushed usage")
			}
		})
	}
	v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.2:1"
	r.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), r)
	v.mu.Lock()
	if len(v.touches) != 1 || v.touches[0] != "12345678" || v.flushes != 0 {
		v.mu.Unlock()
		t.Fatalf("touches=%v flushes=%d", v.touches, v.flushes)
	}
	v.mu.Unlock()

	blockedVerifier := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
	blockedNow := time.Unix(600, 0)
	blockedHandler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: blockedVerifier, Clock: func() time.Time { return blockedNow }}, RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 10; i++ {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.90:1"
		blockedHandler.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.90:1"
	blocked := httptest.NewRecorder()
	blockedHandler.ServeHTTP(blocked, req)
	blockedVerifier.mu.Lock()
	defer blockedVerifier.mu.Unlock()
	if blocked.Code != http.StatusTooManyRequests || len(blockedVerifier.touches) != 0 {
		t.Fatalf("backoff status=%d touches=%v", blocked.Code, blockedVerifier.touches)
	}
}

func TestChainUsesTokenBucketPerToken(t *testing.T) {
	isolateAPIGlobals(t)
	now := time.Unix(100, 0)
	capture := &captureHandler{notify: make(chan struct{}, 8)}
	reached := 0
	v := &multiTokenVerifier{}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, Logger: slog.New(capture), RPS: 1, Burst: 3})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
	request := func(id, peer string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = peer + ":10"
		r.Header.Set("Authorization", "Bearer rush_"+id+"."+strings.Repeat("a", 43))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 3; i++ {
		if w := request("12345678", fmt.Sprintf("192.0.2.%d", i+1)); w.Code != 200 {
			t.Fatalf("burst %d status=%d", i, w.Code)
		}
	}
	if w := request("12345678", "192.0.2.10"); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("limited=%d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := request("87654321", "192.0.2.11"); w.Code != 200 {
		t.Fatalf("second token status=%d", w.Code)
	}
	now = now.Add(time.Second)
	if w := request("12345678", "192.0.2.12"); w.Code != 200 {
		t.Fatalf("refill status=%d", w.Code)
	}
	if reached != 5 {
		t.Fatalf("handler calls=%d want 5", reached)
	}
	records := waitForRecords(t, capture, 6)
	secret := strings.Repeat("a", 43)
	for _, rec := range records {
		text := rec.message + " " + fmt.Sprint(rec.attrs)
		if strings.Contains(text, secret) || strings.Contains(text, "rush_12345678."+secret) {
			t.Fatalf("audit record leaked credential: %s", text)
		}
	}
	found := false
	for _, rec := range records {
		if rec.attrs["status"] == 429 && rec.attrs["token_id"] == "12345678" && rec.attrs["scope"] == "run" {
			found = true
		}
	}
	if !found {
		t.Fatal("429 audit record missing token id")
	}
}

type multiTokenVerifier struct{}

func (*multiTokenVerifier) VerifyAPIToken(token string, _ time.Time) (config.APIToken, bool, error) {
	return config.APIToken{ID: token[5:13], Scopes: []string{"run"}}, true, nil
}
func (*multiTokenVerifier) Touch(string) {}
func (*multiTokenVerifier) ShowAPIToken(id string) (config.APIToken, error) {
	return config.APIToken{ID: id, Scopes: []string{"run"}}, nil
}

func TestChainProxyHeadersDoNotChangePeerBackoffOrAudit(t *testing.T) {
	isolateAPIGlobals(t)
	capture := &captureHandler{notify: make(chan struct{}, 16)}
	now := time.Unix(100, 0)
	v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, Logger: slog.New(capture), RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 10; i++ {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.1:20"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("failure %d status=%d", i, w.Code)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.1:20"
	r.Header.Set("X-Forwarded-For", "198.51.100.99")
	blocked := httptest.NewRecorder()
	h.ServeHTTP(blocked, r)
	if blocked.Code != 429 {
		t.Fatalf("blocked peer status=%d", blocked.Code)
	}
	v.ok = true
	r = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.2:20"
	r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	r.Header.Set("Forwarded", "for=192.0.2.1")
	r.Header.Set("X-Real-IP", "192.0.2.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("spoofed peer status=%d", w.Code)
	}
	records := waitForRecords(t, capture, 12)
	for _, rec := range records {
		if rec.attrs["remote_addr"] != "192.0.2.1:20" && rec.attrs["remote_addr"] != "192.0.2.2:20" {
			t.Fatalf("audit recorded non-peer address %v", rec.attrs["remote_addr"])
		}
	}
	for i, rec := range records {
		want := "192.0.2.1:20"
		if i == 11 {
			want = "192.0.2.2:20"
		}
		if rec.attrs["remote_addr"] != want {
			t.Fatalf("record %d remote_addr=%v want=%s", i, rec.attrs["remote_addr"], want)
		}
	}
}

func TestChainZeroRateDefaultsArePinned(t *testing.T) {
	isolateAPIGlobals(t)
	v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	now := time.Unix(500, 0)
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for i := 0; i < 41; i++ {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", i+1)
		r.Header.Set("Authorization", "Bearer rush_12345678."+strings.Repeat("a", 43))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 200
		if i == 40 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("request %d status=%d want=%d", i, w.Code, want)
		}
	}
}

func TestChainFailureWindowUsesLastActivity(t *testing.T) {
	isolateAPIGlobals(t)
	start := time.Unix(200, 0)
	now := start
	v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: false}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = "192.0.2.44:99"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 401 {
		t.Fatalf("first failure status=%d", w.Code)
	}
	now = start.Add(30 * time.Second)
	if w := request(); w.Code != 401 {
		t.Fatalf("second failure status=%d", w.Code)
	}
	// The first failure is exactly 60s old, so it remains in the rolling
	// window. lastActivity is refreshed by this failure, preventing eviction.
	now = start.Add(60 * time.Second)
	if w := request(); w.Code != 401 {
		t.Fatalf("failure at rolling-window boundary status=%d", w.Code)
	}
	for i := 0; i < 7; i++ {
		if w := request(); w.Code != 401 {
			t.Fatalf("failure %d status=%d", i+4, w.Code)
		}
	}
	if w := request(); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("tenth failure backoff status=%d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestChainRealStoreLastUsedPersistsThroughFlusher(t *testing.T) {
	isolateAPIGlobals(t)
	dir := t.TempDir()
	store := &config.APITokenStore{Path: dir + "/tokens.json"}
	token, record, err := store.CreateAPIToken("touch", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("tokens before request=%d", len(before))
	}
	statePath := filepath.Join(dir, "api-tokens.state")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(stateBefore) != 0 {
		var prior map[string]time.Time
		if err := json.Unmarshal(stateBefore, &prior); err != nil {
			t.Fatal(err)
		}
		if !prior[record.ID].IsZero() {
			t.Fatalf("last_used before request=%s", prior[record.ID])
		}
	}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store}, RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.50:5"
	r.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var usage map[string]time.Time
	if err := json.Unmarshal(state, &usage); err != nil {
		t.Fatal(err)
	}
	if usage[record.ID].IsZero() {
		t.Fatalf("state has no last_used for %s: %s", record.ID, state)
	}
}

func TestChainRealListener429ResponseGoldens(t *testing.T) {
	isolateAPIGlobals(t)
	cases := []struct {
		name     string
		requests int
		backoff  bool
	}{{"rate limited", 4, false}, {"back-off", 11, true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(300, 0)
			v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: !tc.backoff}
			capture := &captureHandler{notify: make(chan struct{}, tc.requests+1)}
			h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v, Clock: func() time.Time { return now }}, Logger: slog.New(capture), RPS: 1, Burst: 3})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			srv := httptest.NewServer(h)
			defer srv.Close()
			address := strings.TrimPrefix(srv.URL, "http://")
			for i := 0; i < tc.requests; i++ {
				auth := "Authorization: Bearer rush_12345678." + strings.Repeat("a", 43) + "\r\n"
				if tc.backoff {
					auth = "Authorization: Bearer malformed\r\n"
				}
				got := normalizeWireDate(rawWireRequest(t, address, "GET / HTTP/1.1\r\nHost: "+address+"\r\n"+auth+"Connection: close\r\n\r\n"))
				want := []byte("HTTP/1.1 200 OK\r\nDate: <normalized>\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
				if (tc.backoff && i >= 10) || (!tc.backoff && i >= 3) {
					want = []byte("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/problem+json\r\nRetry-After: 1\r\nDate: <normalized>\r\nContent-Length: 64\r\nConnection: close\r\n\r\n{\"type\":\"about:blank\",\"title\":\"Too Many Requests\",\"status\":429}\n")
				} else if tc.backoff {
					want = []byte("HTTP/1.1 401 Unauthorized\r\nContent-Type: application/problem+json\r\nWWW-Authenticate: Bearer\r\nDate: <normalized>\r\nContent-Length: 59\r\nConnection: close\r\n\r\n{\"type\":\"about:blank\",\"title\":\"Unauthorized\",\"status\":401}\n")
				}
				if string(got) != string(want) {
					t.Fatalf("request %d wire response=%q want=%q", i, got, want)
				}
			}
			records := waitForRecords(t, capture, tc.requests)
			for _, rec := range records {
				text := rec.message + " " + fmt.Sprint(rec.attrs)
				if strings.Contains(text, strings.Repeat("a", 43)) || strings.Contains(text, "rush_12345678") {
					t.Fatalf("wire audit leaked credential: %s", text)
				}
			}
		})
	}
}

func TestChainRealListenerResponseGoldens(t *testing.T) {
	isolateAPIGlobals(t)
	cases := []struct {
		name, auth, method, body string
		err                      error
	}{
		{"401", "", http.MethodGet, "", nil},
		{"503", "Authorization: Bearer rush_12345678." + strings.Repeat("a", 43) + "\r\n", http.MethodGet, "", errors.New("store")},
		{"413", "Authorization: Bearer rush_12345678." + strings.Repeat("a", 43) + "\r\n", http.MethodPost, "xx", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &recordingVerifier{token: config.APIToken{ID: "12345678"}, ok: true, err: tc.err}
			capture := &captureHandler{notify: make(chan struct{}, 8)}
			h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: slog.New(capture), MaxBody: 1, RPS: 100, Burst: 100})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			srv := httptest.NewServer(h)
			defer srv.Close()
			address := strings.TrimPrefix(srv.URL, "http://")
			request := tc.method + " / HTTP/1.1\r\nHost: " + address + "\r\n" + tc.auth
			if tc.body != "" {
				request += "Content-Length: 2\r\n"
			}
			request += "Connection: close\r\n\r\n" + tc.body
			got := normalizeWireDate(rawWireRequest(t, address, request))
			want := ""
			switch tc.name {
			case "401":
				want = "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/problem+json\r\nWWW-Authenticate: Bearer\r\nDate: <normalized>\r\nContent-Length: 59\r\nConnection: close\r\n\r\n{\"type\":\"about:blank\",\"title\":\"Unauthorized\",\"status\":401}\n"
			case "503":
				want = "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/problem+json\r\nDate: <normalized>\r\nContent-Length: 66\r\nConnection: close\r\n\r\n{\"type\":\"about:blank\",\"title\":\"Service Unavailable\",\"status\":503}\n"
			case "413":
				want = "HTTP/1.1 413 Request Entity Too Large\r\nContent-Type: application/problem+json\r\nDate: <normalized>\r\nContent-Length: 64\r\nConnection: close\r\n\r\n{\"type\":\"about:blank\",\"title\":\"Request Too Large\",\"status\":413}\n"
			}
			if string(got) != want {
				t.Fatalf("wire response=%q want=%q", got, want)
			}
			for _, rec := range waitForRecords(t, capture, 1) {
				text := rec.message + " " + fmt.Sprint(rec.attrs)
				if strings.Contains(text, strings.Repeat("a", 43)) || strings.Contains(text, "rush_12345678") {
					t.Fatalf("wire audit leaked credential: %s", text)
				}
			}
		})
	}
}
