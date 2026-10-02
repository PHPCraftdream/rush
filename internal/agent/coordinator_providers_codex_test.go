package agent

import (
	"testing"

	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProviderDoesNotTreatCodexAPIKeyAsOAuth(t *testing.T) {
	store := config.NewLibraryStore(&config.Config{Options: &config.Options{}}, t.TempDir())
	coord := &coordinator{cfg: store}
	_, err := coord.buildProvider(store.Config(), config.ProviderConfig{
		ID:     "openai-codex",
		Type:   openaicompat.Name,
		APIKey: "must-not-fall-back-to-an-API-key",
	}, config.SelectedModel{}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OAuth access token")
}
