package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

const codexTokenProviderID = "openai-codex"

type codexTokenStore interface {
	Config() *config.Config
	RefreshOAuthTokenWithClient(context.Context, config.Scope, config.ProviderConfig, *http.Client) error
}

type codexTokenSource struct {
	store   codexTokenStore
	resolve func(*config.Config, config.ProviderConfig) (*http.Client, error)
}

func (s *codexTokenSource) Current() (oauth.Token, bool) {
	if s == nil || s.store == nil {
		return oauth.Token{}, false
	}
	cfg := s.store.Config()
	if cfg == nil || cfg.Providers == nil {
		return oauth.Token{}, false
	}
	pc, ok := cfg.Providers.Get(codexTokenProviderID)
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
	current, ok := cfg.Providers.Get(codexTokenProviderID)
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

func newCodexTokenProvider(client *http.Client, source *codexTokenSource) (fantasy.Provider, error) {
	token, ok := source.Current()
	if !ok || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("openai-codex requires an OAuth access token; credentials unavailable, removed or logged out")
	}
	return codexprovider.NewWithSource(client, source)
}
