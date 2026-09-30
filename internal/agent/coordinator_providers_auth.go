package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/notify"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	_, err := c.refreshExpiredToken(ctx, providerCfg)
	return err
}

// refreshExpiredToken is refreshTokenIfExpired that also says whether a
// refresh really happened: a caller that resolved its model BEFORE the refresh
// holds a client built on the old token and must rebuild it.
func (c *coordinator) refreshExpiredToken(ctx context.Context, providerCfg config.ProviderConfig) (bool, error) {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return false, nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildOnCurrentCredentials rebuilds model's provider client from the
// current config snapshot, keeping the model itself pinned (same model, new
// client): the pinned/frozen models of a call resolved before a credential
// refresh, and of a delegated child's driver template, carry the old token.
func (c *coordinator) rebuildOnCurrentCredentials(ctx context.Context, model Model, isSubAgent bool) (Model, error) {
	cfg, providerCfg, err := c.rebuildInputs(model.ModelCfg.Provider)
	if err != nil {
		return Model{}, err
	}
	return c.rebuildPinnedModel(ctx, cfg, model, providerCfg, isSubAgent)
}

// publishReauthenticate asks the operator to re-authenticate with providerID
// when it is the hyper provider (the only one with an interactive login flow
// behind the notification).
func (c *coordinator) publishReauthenticate(providerID string) {
	if c.notify == nil || providerID != hyper.Name {
		return
	}
	c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		Type:       notify.TypeReAuthenticate,
		ProviderID: providerID,
	})
}

// refreshAfterUnauthorized refreshes providerID's credentials after a Drain
// attempt got a 401. It is the ONE place a Drain's 401 refreshes them: the turn
// loop that accounts the attempt calls it before the verdict, because a
// launcher (web wake, delegated child) never sees the error in time and the
// CLI's runInternal returns after the accounting. True means the credentials
// were refreshed (OAuth token or API-key template), so the next attempt can
// succeed and the 401 is a transient failure; false means there is no refresh
// path or it failed, and the 401 stays terminal.
func (c *coordinator) refreshAfterUnauthorized(ctx context.Context, providerID string) bool {
	if c == nil || c.cfg == nil || providerID == "" {
		return false
	}
	_, providerCfg, err := c.rebuildInputs(providerID)
	if err != nil {
		return false
	}
	if err := c.retryAfterUnauthorized(ctx, providerCfg); err != nil {
		slog.Warn("401 on a Drain: credential refresh skipped", "provider", providerID, "error", err)
		return false
	}
	return true
}

// checkLivePeakHours returns the current peak-hours decision for providerID.
// It reloads the config first when a tracked config file changed on disk, so
// long-running agents in one process can observe peak_hours edits made by the
// web UI or CLI in another process.
func (c *coordinator) checkLivePeakHours(providerID string) error {
	if c == nil || c.cfg == nil || providerID == "" {
		return nil
	}
	if staleness := c.cfg.ConfigStaleness(); staleness.Dirty {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.mcpOwner.ReloadAndReconcileMCPConfig(reloadCtx, c.cfg); err != nil {
			slog.Warn("Failed to reload config before peak-hours check", "provider", providerID, "err", err)
		}
		cancel()
	}
	cfg := c.cfg.Config()
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil
	}
	return checkPeakHours(pc)
}

// checkPeakHours refuses the request if providerCfg is currently inside its
// configured peak_hours window. Returns nil (allow) when the window is absent
// or not currently active. The returned error wraps errProviderPeakHours so
// callers and classifyProviderError can identify it via errors.Is.
func checkPeakHours(providerCfg config.ProviderConfig) error {
	w := providerCfg.PeakHours
	if w == nil {
		return nil
	}
	now := time.Now()
	if !w.InPeakHours(now) {
		return nil
	}
	end := w.EndTimeToday(now)
	slog.Warn(
		"Refusing request: provider is inside its peak-hours window",
		"provider", providerCfg.ID,
		"window_start", w.Start,
		"window_end", w.End,
		"available_again", end.Format("15:04"),
		"in", time.Until(end).Round(time.Minute).String(),
	)
	return &PeakHoursError{
		ProviderID: providerCfg.ID,
		Start:      w.Start,
		End:        w.End,
		ReopensAt:  end,
		Message:    w.Message,
	}
}

// runWithUnauthorizedRetry executes fn. If fn returns a 401 error, it
// attempts to refresh credentials and rebuilds the call before retrying.
// Returns the final error: from the retry if a retry was attempted, otherwise
// from the original run. Callers that need to notify the user on persistent
// failure should check isUnauthorized on the returned error.
//
// After credential refresh, rebuildCall is invoked to reconstruct the call
// with fresh credentials, ensuring the retry uses a new provider client
// rather than the stale pinned client from the original attempt (task #341,
// P1-2). A rebuild failure replaces the stale 401 so callers classify the
// terminal local failure correctly.
var errUnauthorizedRefreshUnavailable = errors.New("credential refresh is unavailable")

var errUnauthorizedRebuildUnavailable = errors.New("cannot retry unauthorized request without a call rebuild")

func (c *coordinator) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error, rebuildCall func() error) error {
	err := fn()
	if err != nil && c.isUnauthorized(err) {
		retryErr := c.retryAfterUnauthorized(ctx, providerCfg)
		if retryErr != nil {
			slog.Warn("401 retry skipped: credential refresh failed", "provider", providerCfg.ID, "error", retryErr)
			return err
		}
		// Rebuild with the refreshed provider client before retrying.
		if rebuildCall == nil {
			return errUnauthorizedRebuildUnavailable
		}
		if rebuildErr := rebuildCall(); rebuildErr != nil {
			return rebuildErr
		}
		return fn()
	}
	return err
}

// retryAfterUnauthorized attempts to refresh credentials after receiving a 401
// and returns nil if retry should be attempted. This calls UpdateModels which
// clears the model cache and rebuilds the shared agent with fresh credentials
// (task #341, P1-3).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		return c.refreshOAuth2Token(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return errUnauthorizedRefreshUnavailable
	}
}

func (c *coordinator) isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	var err error
	if c.refreshOAuth2TokenFn != nil {
		err = c.refreshOAuth2TokenFn(ctx, providerCfg)
	} else {
		// R5-2: refresh rides the SAME provider network policy (proxy/DNS
		// transport) inference uses, so an auth endpoint reachable only
		// through the configured route cannot split inference (works)
		// from refresh (fails). resolveProviderHTTPClient
		// returns (nil, nil) when NEITHER a proxy/DNS/DoH override NOR
		// debug logging is configured at all — that nil legitimately
		// means "no policy to enforce", and the helpers' historical
		// default-route client is correct for it. A non-nil clientErr is
		// different: policy WAS configured and its build failed, so
		// silently falling back to the default route here would perform
		// the token exchange over an unconfigured route the operator
		// explicitly meant to require (R5-2 residual, 2026-09-22 round-8
		// audit — the earlier "log and fall back" version of this code
		// did exactly that). Refuse instead of downgrading silently.
		// Pair a fresh same-generation provider entry with cfg. A removed
		// provider has no valid network/token pair to refresh.
		cfg, _ := c.cfg.Snapshot()
		if cfg == nil || cfg.Providers == nil {
			return fmt.Errorf("resolve provider network client for OAuth refresh: provider %s is no longer configured; refusing refresh", providerCfg.ID)
		}
		freshProviderCfg, ok := cfg.Providers.Get(providerCfg.ID)
		if !ok {
			return fmt.Errorf("resolve provider network client for OAuth refresh: provider %s is no longer configured; refusing refresh", providerCfg.ID)
		}
		providerCfg = freshProviderCfg
		httpClient, clientErr := c.resolveProviderHTTPClient(cfg, providerCfg)
		if clientErr != nil {
			return fmt.Errorf("resolve provider network client for OAuth refresh: %w", clientErr)
		}
		// F6 round-12: hand the SAME providerCfg entry the client was
		// built from to the store, so the token it refreshes comes from
		// the exact generation the client came from instead of a fresh,
		// independently-timed store read.
		err = c.cfg.RefreshOAuthTokenWithClient(ctx, config.ScopeGlobal, providerCfg, httpClient)
	}
	if err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	if !c.cfg.SetProviderRuntimeAPIKeyIfTemplate(providerCfg.ID, providerCfg.APIKeyTemplate, providerCfg.APIKey, newAPIKey) {
		return errUnauthorizedRefreshUnavailable
	}

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}
