package agent

// Narrow, agent-package-only test seams for provauth wiring tests: these
// fakes are injected through provauth's exported constructors; no production
// state is mutated and no exported mutable globals are introduced.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
)

type oauthBearerRT func(*http.Request) (*http.Response, error)

func (rt oauthBearerRT) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func oauthBearerResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Original": {"yes"}}, Body: io.NopCloser(strings.NewReader(body))}
}

type rootOAuthBearerSource struct {
	mu        sync.Mutex
	token     oauth.Token
	present   bool
	lookups   atomic.Int32
	refreshes atomic.Int32
	refresh   func(context.Context, oauth.Token) error
}

func (s *rootOAuthBearerSource) Current() (oauth.Token, bool) {
	s.lookups.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, s.present
}

func (s *rootOAuthBearerSource) set(token oauth.Token, present bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token, s.present = token, present
}

func (s *rootOAuthBearerSource) Refresh(ctx context.Context, used oauth.Token) error {
	s.refreshes.Add(1)
	if s.refresh != nil {
		return s.refresh(ctx, used)
	}
	return nil
}
