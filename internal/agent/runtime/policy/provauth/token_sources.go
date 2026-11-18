package provauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
)

const CodexTokenProviderID = "openai-codex"

// oauthTokenStore is the token store the OAuth token source depends on.
// It stays private so callers can satisfy it without naming the interface.
type oauthTokenStore interface {
	Config() *config.Config
	RefreshOAuthTokenWithClient(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

// codexTokenStore is the token store the Codex token source depends on.
// It stays private so callers can satisfy it without naming the interface.
type codexTokenStore interface {
	Config() *config.Config
	RefreshOAuthTokenWithClient(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

type oauthTokenSource struct {
	providerID string
	store      oauthTokenStore
	resolve    func(*config.Config, config.ProviderConfig) (*http.Client, error)
}

// NewOAuthTokenSource builds the OAuth token source; fields stay private so
// callers must inject the store and network resolver here.
func NewOAuthTokenSource(providerID string, store oauthTokenStore, resolve func(*config.Config, config.ProviderConfig) (*http.Client, error)) *oauthTokenSource {
	return &oauthTokenSource{providerID: providerID, store: store, resolve: resolve}
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

type codexTokenSource struct {
	store   codexTokenStore
	resolve func(*config.Config, config.ProviderConfig) (*http.Client, error)
}

// NewCodexTokenSource builds the Codex token source; fields stay private so
// callers must inject the store and network resolver here.
func NewCodexTokenSource(store codexTokenStore, resolve func(*config.Config, config.ProviderConfig) (*http.Client, error)) *codexTokenSource {
	return &codexTokenSource{store: store, resolve: resolve}
}

func (s *codexTokenSource) Current() (oauth.Token, bool) {
	if s == nil || s.store == nil {
		return oauth.Token{}, false
	}
	cfg := s.store.Config()
	if cfg == nil || cfg.Providers == nil {
		return oauth.Token{}, false
	}
	pc, ok := cfg.Providers.Get(CodexTokenProviderID)
	if !ok || pc.OAuthToken == nil {
		return oauth.Token{}, false
	}
	return *pc.OAuthToken, true
}

func (s *codexTokenSource) Refresh(ctx context.Context, used oauth.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.store == nil {
		return errors.New("openai-codex credentials unavailable: provider removed or logged out")
	}
	cfg := s.store.Config()
	if cfg == nil || cfg.Providers == nil {
		return errors.New("openai-codex credentials unavailable: provider removed or logged out")
	}
	current, ok := cfg.Providers.Get(CodexTokenProviderID)
	if !ok || current.OAuthToken == nil {
		return errors.New("openai-codex credentials unavailable: provider removed or logged out")
	}
	if s.resolve == nil {
		return errors.New("openai-codex OAuth refresh network resolver unavailable")
	}
	client, err := s.resolve(cfg, current)
	if err != nil {
		return fmt.Errorf("resolve provider network client for OAuth refresh: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	refreshConfig := current
	refreshConfig.OAuthToken = &used
	return s.store.RefreshOAuthTokenWithClient(ctx, config.ScopeGlobal, refreshConfig, client)
}

// NewCodexTokenProvider is the compiler-proven constructor retained for the
// production caller in package agent.
func NewCodexTokenProvider(client *http.Client, source *codexTokenSource, options ...codexprovider.WebSocketOptions) (fantasy.Provider, error) {
	token, ok := source.Current()
	if !ok || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("openai-codex requires an OAuth access token; credentials unavailable, removed or logged out")
	}
	return codexprovider.NewWithSource(client, source, options...)
}
