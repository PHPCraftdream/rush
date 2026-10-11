package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type oauthCloseCountBody struct {
	closes atomic.Int32
	read   func([]byte) (int, error)
	close  func()
}

func (b *oauthCloseCountBody) Read(p []byte) (int, error) {
	if b.read != nil {
		return b.read(p)
	}
	return 0, io.EOF
}

func (b *oauthCloseCountBody) Close() error {
	b.closes.Add(1)
	if b.close != nil {
		b.close()
	}
	return nil
}

type oauthCloseSource struct {
	current func() (oauth.Token, bool)
	refresh func(context.Context, oauth.Token) error
}

func (s *oauthCloseSource) Current() (oauth.Token, bool) { return s.current() }
func (s *oauthCloseSource) Refresh(ctx context.Context, used oauth.Token) error {
	return s.refresh(ctx, used)
}

func TestOAuthBearerRequestBodyOwnership1297(t *testing.T) {
	cases := []struct {
		name string
		pin  string
	}{
		{"early-context", "top-level ctx.Err rejection closes request"},                          // Revert-check: top-level ctx.Err rejection closes request.
		{"nil-source", "source == nil unavailable closes request"},                               // Revert-check: source == nil unavailable closes request.
		{"missing-token", "!ok unavailable closes request"},                                      // Revert-check: !ok unavailable closes request.
		{"blank-token", "blank AccessToken unavailable closes request"},                          // Revert-check: blank AccessToken unavailable closes request.
		{"send-context", "send ctx.Err rejection closes its request"},                            // Revert-check: send ctx.Err rejection closes its request.
		{"getbody-error", "GetBody error leaves original owned by base"},                         // Revert-check: GetBody error leaves original owned by base.
		{"getbody-error-context", "cancelled after GetBody error leaves original owned by base"}, // Revert-check: cancelled after GetBody error leaves original owned by base.
		{"refresh-context", "cancelled after Refresh leaves original owned by base"},             // Revert-check: cancelled after Refresh leaves original owned by base.
		{"current-context", "cancelled after second Current leaves original owned by base"},      // Revert-check: cancelled after second Current leaves original owned by base.
		{"clone-context", "cancelled after GetBody closes only retry body"},                      // Revert-check: cancelled after GetBody closes only retry body.
		{"drain-context", "ctx.Err after draining closes only retry body"},                       // Revert-check: ctx.Err after draining closes only retry body.
		{"forward-success", "send base.RoundTrip owns original on success"},                      // Revert-check: send base.RoundTrip owns original on success.
		{"forward-error", "send base.RoundTrip owns original on error"},                          // Revert-check: send base.RoundTrip owns original on error.
		{"forward-context", "send postforward ctx.Err does not close original again"},            // Revert-check: send postforward ctx.Err does not close original again.
		{"passthrough", "absent Authorization hands original directly to base"},                  // Revert-check: absent Authorization hands original directly to base.
		{"refresh-error", "refreshErr returns response without reclosing original"},              // Revert-check: refreshErr returns response without reclosing original.
		{"refresh-missing", "!ok after refresh does not reclose original"},                       // Revert-check: !ok after refresh does not reclose original.
		{"refresh-blank", "blank token after refresh does not reclose original"},                 // Revert-check: blank token after refresh does not reclose original.
		{"refresh-unchanged", "unchanged token does not reclose original"},                       // Revert-check: unchanged token does not reclose original.
		{"nonreplayable", "GetBody == nil does not reclose original"},                            // Revert-check: GetBody == nil does not reclose original.
		{"retry-success", "retry send hands new body to base exactly once"},                      // Revert-check: retry send hands new body to base exactly once.
		{"retry-error", "retry send base error leaves new body owned by base"},                   // Revert-check: retry send base error leaves new body owned by base.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Revert-check: tc.pin names the production condition pinned by this case.
			t.Log("Revert-check: " + tc.pin)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original, replay, rejected := &oauthCloseCountBody{}, &oauthCloseCountBody{}, &oauthCloseCountBody{}
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid", original)
			require.NoError(t, err)
			r.Header.Set("Authorization", "caller")
			getBodies, lookups, refreshes, sends := 0, 0, 0, 0
			r.GetBody = func() (io.ReadCloser, error) {
				getBodies++
				if tc.name == "getbody-error" || tc.name == "getbody-error-context" {
					if tc.name == "getbody-error-context" {
						cancel()
					}
					return nil, errors.New("cannot replay")
				}
				if tc.name == "clone-context" {
					cancel()
				}
				return replay, nil
			}
			s := &oauthCloseSource{
				current: func() (oauth.Token, bool) {
					lookups++
					if lookups == 1 {
						switch tc.name {
						case "send-context":
							cancel()
						case "missing-token":
							return oauth.Token{}, false
						case "blank-token":
							return oauth.Token{AccessToken: " \t"}, true
						}
						return oauth.Token{AccessToken: "old"}, true
					}
					switch tc.name {
					case "current-context":
						cancel()
					case "refresh-missing":
						return oauth.Token{}, false
					case "refresh-blank":
						return oauth.Token{AccessToken: " \t"}, true
					case "refresh-unchanged":
						return oauth.Token{AccessToken: "old"}, true
					}
					return oauth.Token{AccessToken: "new"}, true
				},
				refresh: func(context.Context, oauth.Token) error {
					refreshes++
					if tc.name == "refresh-context" {
						cancel()
					}
					if tc.name == "refresh-error" {
						return errors.New("refresh failed")
					}
					return nil
				},
			}
			var source codexprovider.TokenSource = s
			if tc.name == "nil-source" {
				source = nil
			}
			if tc.name == "early-context" {
				cancel()
			}
			if tc.name == "passthrough" {
				r.Header.Del("Authorization")
			}
			if tc.name == "nonreplayable" {
				r.GetBody = nil
			}
			if tc.name == "drain-context" {
				rejected.read = func([]byte) (int, error) { cancel(); return 0, io.EOF }
			}
			baseErr := errors.New("base failed")
			tr := &oauthBearerTransport{source: source, base: oauthBearerRT(func(got *http.Request) (*http.Response, error) {
				sends++
				if sends == 1 {
					require.Same(t, original, got.Body)
				} else {
					require.Same(t, replay, got.Body)
				}
				require.NoError(t, got.Body.Close())
				if tc.name == "forward-error" || (tc.name == "retry-error" && sends == 2) {
					return nil, baseErr
				}
				if tc.name == "forward-context" {
					cancel()
				}
				status := http.StatusUnauthorized
				if tc.name == "forward-success" || tc.name == "passthrough" || sends == 2 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Body: rejected}, nil
			})}
			response, err := tr.RoundTrip(r)
			if strings.Contains(tc.name, "context") {
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, response)
			} else if tc.name == "forward-error" || tc.name == "retry-error" {
				require.ErrorIs(t, err, baseErr)
				require.Nil(t, response)
			} else {
				require.NoError(t, err)
				require.NotNil(t, response)
				require.NoError(t, response.Body.Close())
			}
			require.EqualValues(t, 1, original.closes.Load())
			wantReplay := int32(0)
			if tc.name == "clone-context" || tc.name == "drain-context" || tc.name == "retry-success" || tc.name == "retry-error" {
				wantReplay = 1
			}
			require.Equal(t, wantReplay, replay.closes.Load())
			wantSends := 1
			switch tc.name {
			case "early-context", "nil-source", "missing-token", "blank-token", "send-context":
				wantSends = 0
			case "retry-success", "retry-error":
				wantSends = 2
			}
			require.Equal(t, wantSends, sends)
			wantGetBodies := int(wantReplay)
			if strings.HasPrefix(tc.name, "getbody-error") {
				wantGetBodies = 1
			}
			require.Equal(t, wantGetBodies, getBodies)
			wantRefreshes := 1
			if wantSends == 0 || strings.HasPrefix(tc.name, "forward-") || tc.name == "passthrough" {
				wantRefreshes = 0
			}
			require.Equal(t, wantRefreshes, refreshes)
		})
	}
}

func TestOAuthCloseRequestBodyEmpty1297(t *testing.T) {
	// Closing a request with nil or http.NoBody does not panic.
	for _, body := range []io.ReadCloser{nil, http.NoBody} {
		require.NotPanics(t, func() { closeRequestBody(&http.Request{Body: body}) })
	}
}
