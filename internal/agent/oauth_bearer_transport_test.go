package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type oauthBearerFakeSource struct {
	mu        sync.Mutex
	token     oauth.Token
	present   bool
	lookups   atomic.Int32
	refreshes atomic.Int32
	refresh   func(context.Context, oauth.Token) error
}

func (s *oauthBearerFakeSource) Current() (oauth.Token, bool) {
	s.lookups.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyOAuthToken(s.token), s.present
}

func (s *oauthBearerFakeSource) set(token oauth.Token, present bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token, s.present = token, present
}

func (s *oauthBearerFakeSource) Refresh(ctx context.Context, used oauth.Token) error {
	s.refreshes.Add(1)
	if s.refresh != nil {
		return s.refresh(ctx, used)
	}
	return nil
}

func oauthBearerSource() *oauthBearerFakeSource {
	return &oauthBearerFakeSource{token: oauth.Token{AccessToken: "old", RefreshToken: "refresh", AccountID: "account"}, present: true}
}

type oauthBearerRT func(*http.Request) (*http.Response, error)

func (rt oauthBearerRT) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func oauthBearerResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Original": {"yes"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func oauthBearerRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), "POST", "http://local.invalid", strings.NewReader("payload"))
	require.NoError(t, err)
	r.Header.Set("Authorization", "caller")
	r.Header.Set("X-Keep", "keep")
	return r
}

func TestOAuthBearerRotationAndCallerImmutability(t *testing.T) {
	// Revert-check: per-call Current and request Clone must replace auth without mutating caller headers.
	s := oauthBearerSource()
	var auth []string
	tr := &oauthBearerTransport{source: s, providerID: "copilot", base: oauthBearerRT(func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		require.Equal(t, "keep", r.Header.Get("X-Keep"))
		return oauthBearerResponse(200, "ok"), nil
	})}
	r := oauthBearerRequest(t)
	for _, token := range []string{"old", "rotated"} {
		s.set(oauth.Token{AccessToken: token}, true)
		response, err := tr.RoundTrip(r)
		require.NoError(t, err)
		response.Body.Close()
	}
	require.Equal(t, []string{"Bearer old", "Bearer rotated"}, auth)
	require.Equal(t, "caller", r.Header.Get("Authorization"))
	require.Equal(t, "keep", r.Header.Get("X-Keep"))
	require.Zero(t, s.refreshes.Load())
}

func TestOAuthBearerHTTPRefreshAndSingleRetry(t *testing.T) {
	// Revert-check: RoundTrip's single 401 replay sends the rotated bearer to the HTTP server.
	var goodBearer atomic.Value
	goodBearer.Store("Bearer accepted")
	var sends, rejected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.Header.Get("Authorization") != goodBearer.Load().(string) {
			rejected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "rejected")
			return
		}
		_, _ = io.WriteString(w, "accepted")
	}))
	defer server.Close()
	s := oauthBearerSource()
	s.refresh = func(_ context.Context, used oauth.Token) error {
		require.Equal(t, "old", used.AccessToken)
		s.set(oauth.Token{AccessToken: strings.TrimPrefix(goodBearer.Load().(string), "Bearer ")}, true)
		return nil
	}
	client := server.Client()
	client.Timeout = 3 * time.Second
	client.Transport = &oauthBearerTransport{source: s, base: client.Transport}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader("payload"))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer baked")
	response, err := client.Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "accepted", string(data))
	require.EqualValues(t, 2, sends.Load())
	require.EqualValues(t, 1, rejected.Load())
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestOAuthBearerRefreshAndReplay(t *testing.T) {
	// Revert-check: bearer comparison and refresh guards preserve terminal bodies.
	for _, mode := range []string{"success", "metadata-only", "error", "unchanged", "second401", "missing", "nonreplayable", "getbody-error"} {
		t.Run(mode, func(t *testing.T) {
			s := oauthBearerSource()
			used, _ := s.Current()
			s.refresh = func(_ context.Context, got oauth.Token) error {
				require.Equal(t, used, got)
				next := used
				next.AccessToken = "new"
				if mode == "metadata-only" {
					next = used
					next.ExpiresAt = used.ExpiresAt + 3600
				}
				if mode != "unchanged" {
					s.set(next, mode != "missing")
				}
				if mode == "error" {
					return errors.New("refresh rejected")
				}
				return nil
			}
			sends := 0
			var bodies []string
			first := oauthBearerResponse(401, "first-body") //nolint:bodyclose // Fixture is closed by deferred cleanup and by the transport on replay.
			defer first.Body.Close()
			tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(r *http.Request) (*http.Response, error) {
				sends++
				data, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				bodies = append(bodies, string(data))
				require.Equal(t, "keep", r.Header.Get("X-Keep"))
				if sends == 1 {
					require.Equal(t, "Bearer old", r.Header.Get("Authorization"))
					return first, nil
				}
				require.Equal(t, "Bearer new", r.Header.Get("Authorization"))
				if mode == "second401" {
					return oauthBearerResponse(401, "second-body"), nil
				}
				return oauthBearerResponse(200, "success-body"), nil
			})}
			r := oauthBearerRequest(t)
			originalBody := r.Body
			if mode == "nonreplayable" {
				r.GetBody = nil
			}
			if mode == "getbody-error" {
				r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("cannot replay") }
			}
			response, err := tr.RoundTrip(r)
			require.NoError(t, err)
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			response.Body.Close()
			retry := mode == "success" || mode == "second401"
			if retry {
				require.Equal(t, 2, sends)
				require.Equal(t, []string{"payload", "payload"}, bodies)
				if mode == "second401" {
					require.Equal(t, 401, response.StatusCode)
					require.Equal(t, "second-body", string(data))
				} else {
					require.Equal(t, 200, response.StatusCode)
					require.Equal(t, "success-body", string(data))
				}
			} else {
				require.Same(t, first, response)
				require.Equal(t, 1, sends)
				require.Equal(t, "first-body", string(data))
				require.Equal(t, "yes", response.Header.Get("X-Original"))
			}
			require.EqualValues(t, 1, s.refreshes.Load())
			require.True(t, originalBody == r.Body)
			require.Equal(t, "caller", r.Header.Get("Authorization"))
		})
	}
}

func TestOAuthBearerConcurrentCompareRefresh(t *testing.T) {
	// Revert-check: four rejected snapshots pass the exact used token to compare-refresh and retry once each.
	s := oauthBearerSource()
	var rotations atomic.Int32
	s.refresh = func(_ context.Context, used oauth.Token) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if reflect.DeepEqual(s.token, used) {
			s.token.AccessToken = "new"
			rotations.Add(1)
		}
		return nil
	}
	var oldSends, newSends atomic.Int32
	barrier := make(chan struct{})
	tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer old" {
			if oldSends.Add(1) == 4 {
				close(barrier)
			}
			select {
			case <-barrier:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return oauthBearerResponse(401, "rejected"), nil
		}
		newSends.Add(1)
		return oauthBearerResponse(200, "ok"), nil
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequestWithContext(ctx, "GET", "http://local.invalid", nil)
			r.Header.Set("Authorization", "Bearer baked")
			response, err := tr.RoundTrip(r)
			if err == nil {
				if response.StatusCode != 200 {
					err = errors.New("not retried")
				}
				response.Body.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 4, oldSends.Load())
	require.EqualValues(t, 4, newSends.Load())
	require.EqualValues(t, 4, s.refreshes.Load())
	require.EqualValues(t, 1, rotations.Load())
}

type oauthBearerBlockingBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *oauthBearerBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *oauthBearerBlockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestOAuthBearerCancellation(t *testing.T) {
	// Revert-check: context guards and cancel-safe drain close the rejected body and never retry after cancellation.
	for _, when := range []string{"before", "send", "refresh", "drain"} {
		t.Run(when, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := oauthBearerSource()
			entered := make(chan struct{})
			body := &oauthBearerBlockingBody{entered: entered, closed: make(chan struct{})}
			var sends atomic.Int32
			s.refresh = func(ctx context.Context, _ oauth.Token) error {
				if when == "refresh" {
					close(entered)
					<-ctx.Done()
				}
				s.set(oauth.Token{AccessToken: "new"}, true)
				return nil
			}
			tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if when == "send" {
					close(entered)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 401, Body: body}, nil
			})}
			r := oauthBearerRequest(t).WithContext(ctx)
			if when == "before" {
				cancel()
			}
			done := make(chan error, 1)
			go func() {
				response, err := tr.RoundTrip(r)
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				done <- err
			}()
			if when != "before" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation seam not entered")
				}
				cancel()
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation blocked")
			}
			if when == "before" {
				require.Zero(t, sends.Load())
			} else {
				require.EqualValues(t, 1, sends.Load())
			}
			if when == "drain" || when == "refresh" {
				select {
				case <-body.closed:
				default:
					t.Fatal("rejected body not closed")
				}
			}
		})
	}
}

type oauthBearerCountingBody struct {
	read   int
	closed bool
}

func (b *oauthBearerCountingBody) Read(p []byte) (int, error) { b.read += len(p); return len(p), nil }

func (b *oauthBearerCountingBody) Close() error { b.closed = true; return nil }

func TestOAuthBearerBoundedDrain(t *testing.T) {
	// Revert-check: CopyN caps an infinite rejection body before the single replay.
	s := oauthBearerSource()
	s.refresh = func(context.Context, oauth.Token) error { s.set(oauth.Token{AccessToken: "new"}, true); return nil }
	body := &oauthBearerCountingBody{}
	sends := 0
	tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(*http.Request) (*http.Response, error) {
		sends++
		if sends == 1 {
			return &http.Response{StatusCode: 401, Body: body}, nil
		}
		require.True(t, body.closed)
		return oauthBearerResponse(200, "ok"), nil
	})}
	response, err := tr.RoundTrip(oauthBearerRequest(t))
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, oauthBearerDrainLimit, body.read)
	require.Equal(t, 2, sends)
}

func TestOAuthBearerMissingTokens(t *testing.T) {
	// Revert-check: missing/blank credential guards refuse any outbound send or refresh.
	for _, value := range []struct {
		token   oauth.Token
		present bool
	}{{oauth.Token{}, false}, {oauth.Token{}, true}, {oauth.Token{AccessToken: " \t"}, true}} {
		s := oauthBearerSource()
		s.set(value.token, value.present)
		tr := &oauthBearerTransport{source: s, providerID: "hyper", base: oauthBearerRT(func(*http.Request) (*http.Response, error) { t.Fatal("sent missing token"); return nil, nil })}
		r := oauthBearerRequest(t)
		response, err := tr.RoundTrip(r)
		require.NoError(t, err)
		oauthBearerAssertUnavailable(t, response, r, "hyper")
		require.Zero(t, s.refreshes.Load())
	}
}
