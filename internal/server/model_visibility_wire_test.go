package server

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/require"
)

// TestConfigWireHidesSupersededModelFamilies verifies the web picker's
// config snapshot applies the same family filter as the CLI listings
// (#1171, #1173): glm models below 5.3 and pre-6 gpt models never reach a
// client, while boundary and unparseable ids do.
func TestConfigWireHidesSupersededModelFamilies(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	provider := config.ProviderConfig{
		ID: "zai", Name: "Z.AI", Type: catwalk.TypeOpenAICompat, APIKey: "configured",
		Models: []catwalk.Model{
			{ID: "glm-5.2"},
			{ID: "cc-glm-5.1"},
			{ID: "glm-5.3-flash"},
			{ID: "glm-x"},
		},
	}
	a.Store().SetProviderRuntimeConfig("zai", provider)

	wire, ok := buildConfigWire(a)
	require.True(t, ok)
	models := wire.Providers["zai"].Models
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Equal(t, []string{"glm-5.3-flash", "glm-x"}, ids,
		"the picker must offer exactly the visible glm models")
	require.True(t, discover.ModelVisible("zai", "glm-5.3-flash"),
		"precondition sanity: the boundary version stays visible")
}
