package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestOAuthBearerNoBodyForwarding1302(t *testing.T) {
	s := oauthBearerSource()
	s.refresh = func(context.Context, oauth.Token) error {
		s.set(oauth.Token{AccessToken: "new"}, true)
		return nil
	}
	r := oauthBearerRequest(t)
	require.NoError(t, r.Body.Close())
	r.Body = http.NoBody
	getBodies, sends := 0, 0
	r.GetBody = func() (io.ReadCloser, error) {
		getBodies++
		return &oauthCloseCountBody{}, nil
	}
	tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(got *http.Request) (*http.Response, error) {
		sends++
		require.True(t, got.Body == http.NoBody)
		if sends == 1 {
			require.Equal(t, "Bearer old", got.Header.Get("Authorization"))
			return oauthBearerResponse(http.StatusUnauthorized, "rejected"), nil
		}
		require.Equal(t, "Bearer new", got.Header.Get("Authorization"))
		return oauthBearerResponse(http.StatusOK, "accepted"), nil
	})}
	response, err := tr.RoundTrip(r)
	require.NoError(t, err)
	require.NotNil(t, response)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.True(t, r.Body == http.NoBody)
	require.Equal(t, "caller", r.Header.Get("Authorization"))
	require.Zero(t, getBodies)
	require.Equal(t, 2, sends)
	require.EqualValues(t, 2, s.lookups.Load())
	require.EqualValues(t, 1, s.refreshes.Load())
}

func TestOAuthBearerCancellationOwnership1302(t *testing.T) {
	for _, seam := range []string{"second-current", "getbody", "drain"} {
		t.Run(seam, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			original, replay, rejected := &oauthCloseCountBody{}, &oauthCloseCountBody{}, &oauthCloseCountBody{}
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid", original)
			require.NoError(t, err)
			r.Header.Set("Authorization", "caller")
			getBodies, lookups, refreshes, sends := 0, 0, 0, 0
			r.GetBody = func() (io.ReadCloser, error) {
				getBodies++
				if seam == "getbody" {
					cancel()
				}
				return replay, nil
			}
			if seam == "drain" {
				rejected.read = func([]byte) (int, error) { cancel(); return 0, io.EOF }
			}
			s := &oauthCloseSource{
				current: func() (oauth.Token, bool) {
					lookups++
					if lookups == 1 {
						return oauth.Token{AccessToken: "old"}, true
					}
					if seam == "second-current" {
						cancel()
					}
					return oauth.Token{AccessToken: "new"}, true
				},
				refresh: func(context.Context, oauth.Token) error { refreshes++; return nil },
			}
			tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(got *http.Request) (*http.Response, error) {
				sends++
				require.Same(t, original, got.Body)
				require.NoError(t, got.Body.Close())
				return &http.Response{StatusCode: http.StatusUnauthorized, Body: rejected}, nil
			})}
			response, err := tr.RoundTrip(r)
			if response != nil {
				_ = response.Body.Close()
			}
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, response)
			require.EqualValues(t, 1, original.closes.Load())
			require.Equal(t, 1, sends)
			require.Equal(t, 2, lookups)
			require.Equal(t, 1, refreshes)
			if seam == "second-current" {
				require.Zero(t, getBodies)
				require.Zero(t, replay.closes.Load())
			} else {
				require.Equal(t, 1, getBodies)
				require.EqualValues(t, 1, replay.closes.Load())
			}
		})
	}
}

func TestOAuthBearerGetBodyErrorOwnership1302(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "error"
		if cancelled {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			original, abandoned := &oauthCloseCountBody{}, &oauthCloseCountBody{}
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid", original)
			require.NoError(t, err)
			r.Header.Set("Authorization", "caller")
			getBodies, sends := 0, 0
			r.GetBody = func() (io.ReadCloser, error) {
				getBodies++
				if cancelled {
					cancel()
				}
				return abandoned, errors.New("cannot replay")
			}
			s := oauthBearerSource()
			s.refresh = func(context.Context, oauth.Token) error {
				s.set(oauth.Token{AccessToken: "new"}, true)
				return nil
			}
			first := oauthBearerResponse(http.StatusUnauthorized, "original rejection") //nolint:bodyclose // Fixture is closed by deferred cleanup and read through the returned response.
			defer first.Body.Close()
			tr := &oauthBearerTransport{source: s, base: oauthBearerRT(func(got *http.Request) (*http.Response, error) {
				sends++
				require.Same(t, original, got.Body)
				require.NoError(t, got.Body.Close())
				return first, nil
			})}
			response, err := tr.RoundTrip(r)
			if response != nil {
				defer response.Body.Close()
			}
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, response)
			} else {
				require.NoError(t, err)
				require.Same(t, first, response)
				data, readErr := io.ReadAll(response.Body)
				require.NoError(t, readErr)
				require.Equal(t, "original rejection", string(data))
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			}
			require.EqualValues(t, 1, original.closes.Load())
			require.EqualValues(t, 1, abandoned.closes.Load())
			require.Equal(t, 1, sends)
			require.Equal(t, 1, getBodies)
			require.EqualValues(t, 2, s.lookups.Load())
			require.EqualValues(t, 1, s.refreshes.Load())
		})
	}
}

func TestOAuthBearerBuilderLoggedOut1302(t *testing.T) {
	store := oauthBearerStore(t, "copilot", &oauth.Token{AccessToken: "synthetic"})
	coord := &coordinator{cfg: store}
	pc, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	sends := 0
	client := &http.Client{Transport: oauthBearerRT(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, errors.New("unexpected outbound send")
	})}
	wrapped := coord.withOAuthBearerClient(pc, client)
	require.NotSame(t, client, wrapped)
	source := oauthBearerSource()
	wrapped.Transport.(*oauthBearerTransport).source = source
	provider, err := coord.buildOpenaiCompatProvider(store.Config(), "http://local.invalid", "synthetic", nil, nil, pc.ID, true, wrapped)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	model, err := provider.LanguageModel(ctx, "test-model")
	require.NoError(t, err)
	source.set(oauth.Token{}, false)
	start := time.Now()
	_, err = model.Generate(ctx, fantasy.Call{})
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "copilot OAuth credentials unavailable")
	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(err, &providerErr))
	require.Equal(t, http.StatusUnauthorized, providerErr.StatusCode)
	require.EqualValues(t, 1, source.lookups.Load())
	require.Zero(t, source.refreshes.Load())
	require.Zero(t, sends)
	require.Less(t, elapsed, 2*time.Second)
}
