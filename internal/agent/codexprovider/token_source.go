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
func NewWithSource(client *http.Client, source TokenSource, websocketOptions ...WebSocketOptions) (fantasy.Provider, error) {
	if source == nil {
		return nil, errors.New("openai-codex requires a token source")
	}
	if client == nil {
		client = http.DefaultClient
	}
	p := &provider{client: client, source: source}
	if len(websocketOptions) > 0 {
		options := websocketOptions[0]
		if options.Manager == nil || options.Current == nil {
			return nil, errors.New("openai-codex websocket requires an owner and current policy")
		}
		p.websocket = &options
	}
	return p, nil
}
