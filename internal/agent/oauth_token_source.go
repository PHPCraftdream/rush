package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type oauthTokenStore interface {
	Config() *config.Config
	RefreshOAuthTokenWithClient(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

type oauthTokenSource struct {
	providerID string
	store      oauthTokenStore
	resolve    func(*config.Config, config.ProviderConfig) (*http.Client, error)
}

func (s *oauthTokenSource) Current() (oauth.Token, bool) {
	if s == nil || s.store == nil {
		return oauth.Token{}, false
	}
	cfg := s.store.Config()
	if cfg == nil || cfg.Providers == nil {
		return oauth.Token{}, false
	}
	pc, ok := cfg.Providers.Get(s.providerID)
	if !ok || pc.OAuthToken == nil {
		return oauth.Token{}, false
	}
	return copyOAuthToken(*pc.OAuthToken), true
}

func (s *oauthTokenSource) Refresh(ctx context.Context, used oauth.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return errors.New("OAuth credentials unavailable: no token source")
	}
	if s.store == nil {
		return fmt.Errorf("%s credentials unavailable: provider removed or logged out", s.providerID)
	}
	cfg := s.store.Config()
	if cfg == nil || cfg.Providers == nil {
		return fmt.Errorf("%s credentials unavailable: provider removed or logged out", s.providerID)
	}
	current, ok := cfg.Providers.Get(s.providerID)
	if !ok || current.OAuthToken == nil {
		return fmt.Errorf("%s credentials unavailable: provider removed or logged out", s.providerID)
	}
	if s.resolve == nil {
		return errors.New("OAuth refresh network resolver unavailable")
	}
	client, err := s.resolve(cfg, current)
	if err != nil {
		return fmt.Errorf("resolve provider network client for OAuth refresh: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refreshConfig := current
	used = copyOAuthToken(used)
	refreshConfig.OAuthToken = &used
	return s.store.RefreshOAuthTokenWithClient(ctx, config.ScopeGlobal, refreshConfig, client)
}

func copyOAuthToken(token oauth.Token) oauth.Token {
	if token.Client != nil {
		client := *token.Client
		token.Client = &client
	}
	return token
}

// withOAuthBearerClient leaves SDK/static credentials alone unless a live OAuth store is available.
func (c *coordinator) withOAuthBearerClient(pc config.ProviderConfig, client *http.Client) *http.Client {
	if c == nil || c.cfg == nil || pc.OAuthToken == nil || (pc.ID != "copilot" && pc.ID != "hyper") {
		return client
	}
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied.Transport = &oauthBearerTransport{base: base, providerID: pc.ID, source: &oauthTokenSource{providerID: pc.ID, store: c.cfg, resolve: c.resolveProviderHTTPClient}}
	return &copied
}
