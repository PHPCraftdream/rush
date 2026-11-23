package oauthcred

import (
	"context"
	"fmt"
	"net/http"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/providers/catalog"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
)

// Descriptor describes one OAuth-capable provider: how to exchange a refresh
// token and whether the provider rotates (burns) refresh tokens.
type Descriptor struct {
	id                string
	exchange          func(context.Context, *http.Client, string) (*oauth.Token, error)
	preserveAccountID bool
	rotating          bool
}

func (s *Store) oauthProviderDescriptors() []Descriptor {
	if override := s.descriptorsOverride(); override != nil {
		return override()
	}
	// Copilot reuses its credential; Hyper's rotation contract is unknown.
	return []Descriptor{
		{
			id: "openai-codex", rotating: true, preserveAccountID: true,
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return s.codexRefreshTokenFn()(ctx, client, refresh)
			},
		},
		{
			id: string(catwalk.InferenceProviderCopilot),
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return s.copilotRefreshTokenFn()(ctx, client, refresh)
			},
		},
		{
			id: catalog.HyperName,
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return s.hyperExchangeTokenFn()(ctx, client, refresh)
			},
		},
	}
}

func (s *Store) oauthProvider(id string) (Descriptor, bool) {
	for _, descriptor := range s.oauthProviderDescriptors() {
		if descriptor.id == id {
			return descriptor, true
		}
	}
	return Descriptor{}, false
}

func (s *Store) rotatingOAuthProvider(id string) bool {
	descriptor, ok := s.oauthProvider(id)
	return ok && descriptor.rotating
}

// An unchanged or empty refresh token is success without a digest: the server burned
// nothing, so a journal record carries a predecessor digest iff the refresh token changed
// (oauthRotationDigest). Rejecting it would discard a valid access token.
func normalizeOAuthRefresh(descriptor Descriptor, previous, token *oauth.Token) *oauth.Token {
	if token == nil {
		return nil
	}
	copyToken := *token
	if copyToken.RefreshToken == "" {
		copyToken.RefreshToken = previous.RefreshToken
	}
	if descriptor.preserveAccountID && copyToken.AccountID == "" {
		copyToken.AccountID = previous.AccountID
	}
	return &copyToken
}

// Legacy callers serialize before re-entering their unchanged disk preflight.
func (s *Store) refreshLegacyOAuthToken(ctx context.Context, scope cfgmodel.Scope, pc cfgmodel.ProviderConfig, client *http.Client) error {
	waitCtx, cancel := context.WithTimeout(ctx, s.port.CodexRefreshLockWait())
	defer cancel()
	key := codexRefreshKey{s, pc.ID}
	for {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		codexRefreshActive.Lock()
		if done, busy := codexRefreshActive.calls[key]; busy {
			codexRefreshActive.Unlock()
			select {
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		codexRefreshActive.calls[key] = done
		codexRefreshActive.Unlock()
		defer func() {
			codexRefreshActive.Lock()
			delete(codexRefreshActive.calls, key)
			close(done)
			codexRefreshActive.Unlock()
		}()
		break
	}
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	lockPath := oauthjournal.RefreshLockPath(path, pc.ID)
	lock, err := cfgcommit.AcquireConfigFileLock(waitCtx, lockPath)
	if err != nil {
		return fmt.Errorf("lock OAuth refresh: %w", err)
	}
	defer cfgcommit.ReleaseConfigFileLock(lockPath, lock)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.refreshLegacyOAuthLeader(ctx, scope, pc, client)
}
