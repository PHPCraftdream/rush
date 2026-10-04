// Model-visibility load tests (#1171, #1173): a model explicitly selected
// in config keeps working — and keeps loading — even though the family
// filter hides it from every list.
package config

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// TestLoadKeepsExplicitlySelectedHiddenGLMModel selects zai/glm-5 (hidden
// by the glm-5.3 minimum) the way `rush models use zai/glm-5` would persist
// it, and requires provider configuration plus the effective-slot
// computation to keep the selection: hiding a model from the lists must
// never break the config of someone who pinned it.
func TestLoadKeepsExplicitlySelectedHiddenGLMModel(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Models[SelectedModelTypeSmart] = SelectedModel{Provider: "zai", Model: "glm-5"}
	envMap := env.NewFromMap(map[string]string{"ZAI_API_KEY": "zai-key"})
	resolver := NewShellVariableResolver(envMap)
	known := []catwalk.Provider{zaiKnownProvider()} // Models: [{ID: "glm-5", ...}]

	require.True(t, !discover.ModelVisible("zai", "glm-5"),
		"precondition: the selected model is hidden from the lists")
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), envMap, resolver, known))
	require.NoError(t, configureSelectedModels(testStore(cfg), cfg, known, false),
		"an explicitly pinned hidden model must not break config loading")

	smart := cfg.Models[SelectedModelTypeSmart]
	require.Equal(t, "zai", smart.Provider)
	require.Equal(t, "glm-5", smart.Model)

	provider, ok := cfg.Providers.Get("zai")
	require.True(t, ok)
	require.True(t, containsModel(provider.Models, "glm-5"),
		"the provider catalog keeps the hidden model so the pinned selection resolves with real metadata")
}

// TestLoadKeepsExplicitlySelectedHiddenCodexModel selects
// openai-codex/gpt-5.6-luna (hidden, and with the account catalog
// unavailable, absent from the provider's model list entirely) and requires
// the load to succeed with the pinned selection kept through the
// unverified-passthrough path.
func TestLoadKeepsExplicitlySelectedHiddenCodexModel(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Options.DisableDefaultProviders = true
	cfg.Models[SelectedModelTypeSmart] = SelectedModel{Provider: "openai-codex", Model: "gpt-5.6-luna"}
	cfg.Providers.Set("openai-codex", ProviderConfig{
		ID:         "openai-codex",
		OAuthToken: &oauth.Token{AccessToken: "rush-access-token", AccountID: "chatgpt-account"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := NewShellVariableResolver(env.NewFromMap(nil))
	require.True(t, !discover.ModelVisible("openai-codex", "gpt-5.6-luna"),
		"precondition: the selected model is hidden from the lists")
	require.NoError(t, cfg.configureProviders(ctx, testStore(cfg), env.NewFromMap(nil), resolver, nil))
	require.NoError(t, configureSelectedModels(testStore(cfg), cfg, nil, false),
		"an explicitly pinned hidden Codex model must not break config loading")

	smart := cfg.Models[SelectedModelTypeSmart]
	require.Equal(t, "openai-codex", smart.Provider)
	require.Equal(t, "gpt-5.6-luna", smart.Model)
}

// TestLoadResolvesReviewerGLM53Flash pins the operator's current reviewer
// slot (zai/glm-5.3-flash) and requires a load to keep it untouched: the
// GLM-5.3 minimum must not hide its own boundary version, and the reviewer
// slot is not second-guessed at load time even when the model is absent
// from the catwalk-only catalog.
func TestLoadResolvesReviewerGLM53Flash(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Models[SelectedModelTypeReviewer] = SelectedModel{Provider: "zai", Model: "glm-5.3-flash"}
	envMap := env.NewFromMap(map[string]string{"ZAI_API_KEY": "zai-key"})
	resolver := NewShellVariableResolver(envMap)
	known := []catwalk.Provider{zaiKnownProvider()}

	require.True(t, discover.ModelVisible("zai", "glm-5.3-flash"),
		"glm-5.3-flash sits at the visibility boundary and stays visible")
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), envMap, resolver, known))
	require.NoError(t, configureSelectedModels(testStore(cfg), cfg, known, false),
		"a reviewer slot on glm-5.3-flash must not break config loading")

	reviewer := cfg.Models[SelectedModelTypeReviewer]
	require.Equal(t, "zai", reviewer.Provider)
	require.Equal(t, "glm-5.3-flash", reviewer.Model)
}

func containsModel(models []catwalk.Model, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}
