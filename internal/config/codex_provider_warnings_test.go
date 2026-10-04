package config

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// captureProviderLoadLog runs configureProviders for one openai-codex entry
// that reaches the custom-provider validation loop (no known provider list is
// supplied, as in the custom-providers-only load) and returns the log output.
func captureProviderLoadLog(t *testing.T, provider ProviderConfig) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults("/tmp", "")
	cfg.Providers.Set("openai-codex", provider)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network: discovery is skipped, only validation runs
	resolver := NewShellVariableResolver(env.NewFromMap(nil))
	require.NoError(t, cfg.configureProviders(ctx, testStore(cfg), env.NewFromMap(nil), resolver, nil))
	return buf.String()
}

// An account authenticated by an OAuth token has no API key and its endpoint is
// fixed in code: "missing API key" / "missing API endpoint" is not a finding
// for it and must not be printed.
//
// Revert-check: remove the OAuth Codex branch in load_providers.go's custom
// provider validation and both warnings come back.
func TestConfigureProviders_CodexOAuthEntryDoesNotWarnAboutKeyOrEndpoint(t *testing.T) {
	logs := captureProviderLoadLog(t, ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
	})

	require.NotContains(t, logs, "Provider is missing API key")
	require.NotContains(t, logs, "Skipping custom provider due to missing API endpoint")
}

// Control: the same entry WITHOUT a token is a real misconfiguration and keeps
// warning about both.
func TestConfigureProviders_CodexWithoutOAuthStillWarns(t *testing.T) {
	logs := captureProviderLoadLog(t, ProviderConfig{ID: "openai-codex"})

	require.Contains(t, logs, "Provider is missing API key")
	require.Contains(t, logs, "Skipping custom provider due to missing API endpoint")
}
