package codexprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type sourceSnapshot struct {
	token   oauth.Token
	present bool
}
type testTokenSource struct {
	snapshot    atomic.Pointer[sourceSnapshot]
	refreshes   atomic.Int32
	rotations   atomic.Int32
	currentHook func()
	refreshHook func(context.Context, oauth.Token) error
}

func (s *testTokenSource) set(token oauth.Token, present bool) {
	s.snapshot.Store(&sourceSnapshot{token, present})
}

func (s *testTokenSource) Current() (oauth.Token, bool) {
	value := s.snapshot.Load()
	if s.currentHook != nil {
		s.currentHook()
	}
	return value.token, value.present
}

func (s *testTokenSource) Refresh(ctx context.Context, used oauth.Token) error {
	s.refreshes.Add(1)
	if s.refreshHook != nil {
		return s.refreshHook(ctx, used)
	}
	return nil
}

func newTestSource() *testTokenSource {
	s := &testTokenSource{}
	s.set(oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "old-account"}, true)
	return s
}

func rotatedToken() oauth.Token {
	return oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", AccountID: "new-account"}
}

func sourceModel(t *testing.T, s TokenSource, rt codexRoundTripper) fantasy.LanguageModel {
	t.Helper()
	p, err := NewWithSource(&http.Client{Transport: rt}, s)
	require.NoError(t, err)
	m, err := p.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	return m
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Test": []string{"original"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const successfulSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func collectSourceStream(ctx context.Context, m fantasy.LanguageModel) ([]fantasy.StreamPart, error) {
	stream, err := m.Stream(ctx, fantasy.Call{})
	if err != nil {
		return nil, err
	}
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
		if part.Type == fantasy.StreamPartTypeError {
			err = part.Error
		}
	}
	return parts, err
}

func requireOriginal401(t *testing.T, err error, message string) {
	t.Helper()
	var pe *fantasy.ProviderError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, http.StatusUnauthorized, pe.StatusCode)
	require.Equal(t, message, string(pe.ResponseBody))
	require.Equal(t, "original", pe.ResponseHeaders["X-Test"])
}

func TestSourceRotationBetweenSends(t *testing.T) {
	// Revert-check: removing acquireResponse's per-send Current snapshot leaves stale headers.
	s := newTestSource()
	var auth, accounts []string
	m := sourceModel(t, s, func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		accounts = append(accounts, r.Header.Get("chatgpt-account-id"))
		return testHTTPResponse(200, successfulSSE), nil
	})
	_, err := collectSourceStream(t.Context(), m)
	require.NoError(t, err)
	s.set(rotatedToken(), true)
	_, err = collectSourceStream(t.Context(), m)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer old-access", "Bearer new-access"}, auth)
	require.Equal(t, []string{"old-account", "new-account"}, accounts)
	require.Zero(t, s.refreshes.Load())
}

func TestSourceRefreshSuccessRetriesOnce(t *testing.T) {
	// Revert-check: removing acquireResponse's changed-token retry prevents the successful stream.
	s := newTestSource()
	var sends int
	var payloads []string
	var closed atomic.Bool
	s.refreshHook = func(_ context.Context, used oauth.Token) error {
		require.True(t, closed.Load())
		require.Equal(t, "old-access", used.AccessToken)
		s.set(rotatedToken(), true)
		return nil
	}
	m := sourceModel(t, s, func(r *http.Request) (*http.Response, error) {
		sends++
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		payloads = append(payloads, string(data))
		if sends == 1 {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: &trackedBody{Reader: strings.NewReader(strings.Repeat("x", maxErrorBody+10)), closed: &closed}}, nil
		}
		require.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
		return testHTTPResponse(200, successfulSSE), nil
	})
	parts, err := collectSourceStream(t.Context(), m)
	require.NoError(t, err)
	var deltas, finishes int
	for _, p := range parts {
		if p.Type == fantasy.StreamPartTypeTextDelta {
			deltas++
		}
		if p.Type == fantasy.StreamPartTypeFinish {
			finishes++
		}
	}
	require.Equal(t, 1, deltas)
	require.Equal(t, 1, finishes)
	require.Equal(t, 2, sends)
	require.Equal(t, payloads[0], payloads[1])
	require.EqualValues(t, 1, s.refreshes.Load())
}

type trackedBody struct {
	*strings.Reader
	closed *atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(b.Len() == 0); return nil }

func TestSourceRefreshErrorPreservesOriginal401(t *testing.T) {
	// Revert-check: removing acquireResponse's refreshErr guard retries despite a failed refresh.
	s := newTestSource()
	s.refreshHook = func(context.Context, oauth.Token) error {
		s.set(rotatedToken(), true)
		return errors.New("refresh failed")
	}
	sends := 0
	m := sourceModel(t, s, func(*http.Request) (*http.Response, error) { sends++; return testHTTPResponse(401, "first"), nil })
	_, err := collectSourceStream(t.Context(), m)
	requireOriginal401(t, err, "first")
	require.Equal(t, 1, sends)
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestSourceUnchangedTokenPreservesOriginal401(t *testing.T) {
	// Revert-check: removing acquireResponse's DeepEqual guard retries unchanged credentials.
	s := newTestSource()
	sends := 0
	m := sourceModel(t, s, func(*http.Request) (*http.Response, error) { sends++; return testHTTPResponse(401, "first"), nil })
	_, err := collectSourceStream(t.Context(), m)
	requireOriginal401(t, err, "first")
	require.Equal(t, 1, sends)
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestSourceSecond401StopsAfterTwoSends(t *testing.T) {
	// Revert-check: removing acquireResponse's attempt guard refreshes the second 401.
	s := newTestSource()
	s.refreshHook = func(context.Context, oauth.Token) error {
		token := rotatedToken()
		if s.refreshes.Load() > 1 {
			token.AccessToken = "third-access"
		}
		s.set(token, true)
		return nil
	}
	sends := 0
	m := sourceModel(t, s, func(*http.Request) (*http.Response, error) {
		sends++
		if sends == 1 {
			return testHTTPResponse(401, "first"), nil
		}
		if sends > 2 {
			return testHTTPResponse(200, successfulSSE), nil
		}
		return testHTTPResponse(401, "last"), nil
	})
	_, err := collectSourceStream(t.Context(), m)
	requireOriginal401(t, err, "last")
	require.Equal(t, 2, sends)
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestSourceConcurrent401CompareRefresh(t *testing.T) {
	// Revert-check: removing acquireResponse's single retry guard violates per-call send bounds.
	s := newTestSource()
	var mu sync.Mutex
	s.refreshHook = func(_ context.Context, used oauth.Token) error {
		mu.Lock()
		defer mu.Unlock()
		current, _ := s.Current()
		if reflect.DeepEqual(current, used) {
			s.set(rotatedToken(), true)
			s.rotations.Add(1)
		}
		return nil
	}
	var oldSends, newSends atomic.Int32
	barrier := make(chan struct{})
	m := sourceModel(t, s, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer old-access" {
			if oldSends.Add(1) == 4 {
				close(barrier)
			}
			<-barrier
			return testHTTPResponse(401, "first"), nil
		}
		newSends.Add(1)
		return testHTTPResponse(200, successfulSSE), nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := collectSourceStream(t.Context(), m); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 4, oldSends.Load())
	require.EqualValues(t, 4, newSends.Load())
	require.EqualValues(t, 4, s.refreshes.Load())
	require.EqualValues(t, 1, s.rotations.Load())
}

type failingStreamBody struct {
	reader *strings.Reader
	err    error
}

func (b *failingStreamBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		return n, b.err
	}
	return n, err
}
func (*failingStreamBody) Close() error { return nil }
func TestSourceSyntheticPostYield401NeverRetries(t *testing.T) {
	// Revert-check: moving SSE consumption into acquireResponse's retry path refreshes a post-yield 401.
	s := newTestSource()
	sends := 0
	failure := &fantasy.ProviderError{StatusCode: 401, Message: "synthetic body failure"}
	m := sourceModel(t, s, func(*http.Request) (*http.Response, error) {
		sends++
		if sends > 1 {
			return testHTTPResponse(200, successfulSSE), nil
		}
		return &http.Response{StatusCode: 200, Body: &failingStreamBody{strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"), failure}}, nil
	})
	parts, err := collectSourceStream(t.Context(), m)
	deltas := 0
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			deltas++
		}
	}
	require.Equal(t, 1, deltas)
	require.Same(t, failure, err)
	deltaIndex, errorIndex := -1, -1
	for i, p := range parts {
		if p.Type == fantasy.StreamPartTypeTextDelta {
			deltaIndex = i
		}
		if p.Type == fantasy.StreamPartTypeError {
			errorIndex = i
		}
	}
	require.GreaterOrEqual(t, deltaIndex, 0)
	require.Greater(t, errorIndex, deltaIndex)
	require.Equal(t, 1, sends)
	require.Zero(t, s.refreshes.Load())
}

func TestSourceCancellationDuringRefresh(t *testing.T) {
	// Revert-check: removing acquireResponse's post-refresh ctx guard hides cancellation behind the 401.
	s := newTestSource()
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.refreshHook = func(ctx context.Context, _ oauth.Token) error {
		close(entered)
		<-ctx.Done()
		s.set(rotatedToken(), true)
		return nil
	}
	var sends atomic.Int32
	m := sourceModel(t, s, func(*http.Request) (*http.Response, error) { sends.Add(1); return testHTTPResponse(401, "first"), nil })
	done := make(chan error, 1)
	go func() { _, err := collectSourceStream(ctx, m); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh cancellation did not finish")
	}
	require.EqualValues(t, 1, sends.Load())
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestSourceSnapshotKeepsHeadersCoherent(t *testing.T) {
	// Revert-check: replacing send's token snapshot with separate Current reads mixes account generations.
	s := newTestSource()
	s.currentHook = func() { s.set(rotatedToken(), true) }
	m := sourceModel(t, s, func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer old-access", r.Header.Get("Authorization"))
		require.Equal(t, "old-account", r.Header.Get("chatgpt-account-id"))
		return testHTTPResponse(200, successfulSSE), nil
	})
	_, err := collectSourceStream(t.Context(), m)
	require.NoError(t, err)
	require.Zero(t, s.refreshes.Load())
}

func TestSourceMissingTokenMakesNoHTTPSends(t *testing.T) {
	// Revert-check: removing acquireResponse's token validation sends an unauthenticated request.
	for _, tc := range []struct {
		name    string
		token   oauth.Token
		present bool
	}{{"missing", oauth.Token{}, false}, {"empty", oauth.Token{}, true}, {"blank", oauth.Token{AccessToken: " \t"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSource()
			s.set(tc.token, tc.present)
			sends := 0
			m := sourceModel(t, s, func(*http.Request) (*http.Response, error) { sends++; return testHTTPResponse(401, "unexpected"), nil })
			_, err := collectSourceStream(t.Context(), m)
			require.EqualError(t, err, "openai-codex OAuth credentials unavailable: provider removed, logged out, or access token empty")
			var pe *fantasy.ProviderError
			require.False(t, errors.As(err, &pe))
			require.Zero(t, sends)
			require.Zero(t, s.refreshes.Load())
		})
	}
}

func TestStaticNewCopiesTokenAndCannotRefresh(t *testing.T) {
	// Revert-check: replacing New's static token copy with caller storage changes its credentials.
	token := oauth.Token{AccessToken: "static", AccountID: "static-account"}
	sends := 0
	p, err := New(&http.Client{Transport: codexRoundTripper(func(r *http.Request) (*http.Response, error) {
		sends++
		require.Equal(t, "Bearer static", r.Header.Get("Authorization"))
		require.Equal(t, "static-account", r.Header.Get("chatgpt-account-id"))
		return testHTTPResponse(401, "static rejection"), nil
	})}, &token)
	require.NoError(t, err)
	token.AccessToken = "mutated"
	token.AccountID = "mutated"
	m, err := p.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	_, err = collectSourceStream(t.Context(), m)
	requireOriginal401(t, err, "static rejection")
	require.Equal(t, 1, sends)
	require.EqualError(t, staticTokenSource{}.Refresh(t.Context(), oauth.Token{}), "openai-codex static token refresh is unsupported")
}
