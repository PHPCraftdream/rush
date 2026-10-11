package config

import (
	"context"
	"fmt"
	"net/http"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

type oauthProviderDescriptor struct {
	id                string
	exchange          func(context.Context, *http.Client, string) (*oauth.Token, error)
	preserveAccountID bool
	rotating          bool
}

// Serial tests may replace descriptors; restore only after workers join.
var oauthProviderDescriptorsOverride func() []oauthProviderDescriptor

func oauthProviderDescriptors() []oauthProviderDescriptor {
	if oauthProviderDescriptorsOverride != nil {
		return oauthProviderDescriptorsOverride()
	}
	// Copilot reuses its credential; Hyper's rotation contract is unknown.
	return []oauthProviderDescriptor{
		{
			id: "openai-codex", rotating: true, preserveAccountID: true,
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return codexRefreshTokenFn(ctx, client, refresh)
			},
		},
		{
			id: string(catwalk.InferenceProviderCopilot),
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return copilotRefreshTokenFn(ctx, client, refresh)
			},
		},
		{
			id: hyperp.Name,
			exchange: func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error) {
				return hyperExchangeTokenFn(ctx, client, refresh)
			},
		},
	}
}

func oauthProvider(id string) (oauthProviderDescriptor, bool) {
	for _, descriptor := range oauthProviderDescriptors() {
		if descriptor.id == id {
			return descriptor, true
		}
	}
	return oauthProviderDescriptor{}, false
}

func rotatingOAuthProvider(id string) bool {
	descriptor, ok := oauthProvider(id)
	return ok && descriptor.rotating
}

func normalizeOAuthRefresh(descriptor oauthProviderDescriptor, previous, token *oauth.Token) *oauth.Token {
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
func (s *ConfigStore) refreshLegacyOAuthToken(ctx context.Context, scope Scope, pc ProviderConfig, client *http.Client) error {
	waitCtx, cancel := context.WithTimeout(ctx, codexRefreshLockWait)
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
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	lockPath := oauthRefreshLockPath(path, pc.ID)
	lock, err := acquireConfigFileLock(waitCtx, lockPath)
	if err != nil {
		return fmt.Errorf("lock OAuth refresh: %w", err)
	}
	defer releaseConfigFileLock(lockPath, lock)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.refreshLegacyOAuthLeader(ctx, scope, pc, client)
}
