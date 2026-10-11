package codexprovider

import (
	"context"
	"errors"
	"net/http"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

// TokenSource supplies memory snapshots and refreshes rejected credentials.
type TokenSource interface {
	Current() (oauth.Token, bool)
	Refresh(context.Context, oauth.Token) error
}

type staticTokenSource struct{ token oauth.Token }

func (s staticTokenSource) Current() (oauth.Token, bool) { return s.token, true }
func (s staticTokenSource) Refresh(context.Context, oauth.Token) error {
	return errors.New("openai-codex static token refresh is unsupported")
}

// NewWithSource creates a provider that snapshots credentials for each send.
func NewWithSource(client *http.Client, source TokenSource) (fantasy.Provider, error) {
	if source == nil {
		return nil, errors.New("openai-codex requires a token source")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &provider{client: client, source: source}, nil
}
