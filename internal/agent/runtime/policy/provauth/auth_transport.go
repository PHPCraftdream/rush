// Package provauth holds the provider auth transports and OAuth token
// sources split out of package agent (chunk B: auth only).
package provauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
)

// dropEmptyAuthTransport removes Authorization / X-Api-Key headers whose value
// is empty. configureAnthropicAuthHeaders blanks the unused one to keep the
// process environment out of the request; the SDK then puts the blank header
// on the wire, which no endpoint should have to tolerate.
type dropEmptyAuthTransport struct{ base http.RoundTripper }

func (t dropEmptyAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var empty []string
	for _, name := range []string{"Authorization", "X-Api-Key"} {
		if vals, ok := req.Header[name]; ok && allBlank(vals) {
			empty = append(empty, name)
		}
	}
	if len(empty) > 0 {
		req = req.Clone(req.Context())
		for _, name := range empty {
			req.Header.Del(name)
		}
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func allBlank(vals []string) bool {
	for _, v := range vals {
		if v != "" {
			return false
		}
	}
	return true
}

// WithoutEmptyAuthHeaders returns a copy of client (nil = the default client)
// whose transport drops blank auth headers; the shared client is not mutated.
func WithoutEmptyAuthHeaders(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Transport: dropEmptyAuthTransport{}}
	}
	clone := *client
	clone.Transport = dropEmptyAuthTransport{base: client.Transport}
	return &clone
}

const oauthBearerDrainLimit = 4 << 10

// oauthBearerTransport snapshots memory credentials and retries only a rejected,
// replayable request after a successful credential rotation.
type oauthBearerTransport struct {
	base       http.RoundTripper
	source     codexprovider.TokenSource
	providerID string
}

func (t *oauthBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if _, owns := request.Header["Authorization"]; !owns {
		return base.RoundTrip(request)
	}
	ctx := request.Context()
	if err := ctx.Err(); err != nil {
		closeRequestBody(request)
		return nil, err
	}
	unavailable := func() (*http.Response, error) {
		closeRequestBody(request)
		body, _ := json.Marshal(map[string]any{"error": map[string]string{
			"message": t.providerID + " OAuth credentials unavailable: provider removed or logged out",
			"type":    "authentication_error",
		}})
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Request:    request,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	}
	if t.source == nil {
		return unavailable()
	}
	used, ok := t.source.Current()
	if !ok || strings.TrimSpace(used.AccessToken) == "" {
		return unavailable()
	}
	send := func(r *http.Request, access string) (*http.Response, error) {
		if _, owns := r.Header["Authorization"]; !owns {
			return base.RoundTrip(r)
		}
		if err := ctx.Err(); err != nil {
			closeRequestBody(r)
			return nil, err
		}
		cloned := r.Clone(ctx)
		if cloned.Header == nil {
			cloned.Header = make(http.Header)
		}
		cloned.Header.Set("Authorization", "Bearer "+access)
		response, err := base.RoundTrip(cloned)
		if ctx.Err() != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return nil, ctx.Err()
		}
		return response, err
	}
	response, err := send(request, used.AccessToken)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	cancelled := func() error {
		if err := ctx.Err(); err != nil {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			return err
		}
		return nil
	}
	refreshErr := t.source.Refresh(ctx, used)
	if err := cancelled(); err != nil {
		return nil, err
	}
	if refreshErr != nil {
		return response, nil
	}
	current, ok := t.source.Current()
	if err := cancelled(); err != nil {
		return nil, err
	}
	if !ok || strings.TrimSpace(current.AccessToken) == "" || current.AccessToken == used.AccessToken {
		return response, nil
	}
	retry := request.Clone(ctx)
	if request.Body != nil && request.Body != http.NoBody {
		if request.GetBody == nil {
			return response, nil
		}
		body, err := request.GetBody()
		if err != nil {
			closeRequestBody(&http.Request{Body: body})
			if err := cancelled(); err != nil {
				return nil, err
			}
			return response, nil
		}
		retry.Body = body
	}
	if err := cancelled(); err != nil {
		closeRequestBody(retry)
		return nil, err
	}
	if response.Body != nil {
		stop := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
		_, _ = io.CopyN(io.Discard, response.Body, oauthBearerDrainLimit)
		stop()
		_ = response.Body.Close()
	}
	if err := ctx.Err(); err != nil {
		closeRequestBody(retry)
		return nil, err
	}
	return send(retry, current.AccessToken)
}

func closeRequestBody(request *http.Request) {
	if request.Body != nil && request.Body != http.NoBody {
		_ = request.Body.Close()
	}
}

func (t *oauthBearerTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// BearerTransport aliases the package-private bearer transport so callers in
// package agent can type-assert wrapped clients; fields stay private and the
// constructor below is the only way to build one from outside the package.
type BearerTransport = oauthBearerTransport

// NewBearerTransport builds the OAuth bearer transport; base and source are
// snapshotted exactly as the previous composite literal did.
func NewBearerTransport(base http.RoundTripper, source codexprovider.TokenSource, providerID string) *oauthBearerTransport {
	return &oauthBearerTransport{base: base, source: source, providerID: providerID}
}

// Base exposes the wrapped base transport read-only for wiring assertions.
func (t *oauthBearerTransport) Base() http.RoundTripper { return t.base }
