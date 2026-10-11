package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
)

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
		return nil, err
	}
	unavailable := func() (*http.Response, error) {
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
			if err := cancelled(); err != nil {
				return nil, err
			}
			return response, nil
		}
		retry.Body = body
	}
	if err := cancelled(); err != nil {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		return nil, err
	}
	if response.Body != nil {
		stop := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
		_, _ = io.CopyN(io.Discard, response.Body, oauthBearerDrainLimit)
		stop()
		_ = response.Body.Close()
	}
	if err := ctx.Err(); err != nil {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		return nil, err
	}
	return send(retry, current.AccessToken)
}

func (t *oauthBearerTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
