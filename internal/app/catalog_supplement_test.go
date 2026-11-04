package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestResolveModel_SupplementalAnthropicModelIsKnown(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	store, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	a := &App{config: store}
	provider, model, known, err := a.ResolveModel("anthropic/claude-haiku-5-5")
	require.NoError(t, err)
	require.Equal(t, "anthropic", provider)
	require.Equal(t, "claude-haiku-5-5", model)
	require.True(t, known)
}
