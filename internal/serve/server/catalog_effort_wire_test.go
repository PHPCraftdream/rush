package server

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestConfigWireOnlyOffersProviderReportedZAIEfforts(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	provider := config.ProviderConfig{
		ID: "zai", Name: "Z.AI", Type: catwalk.TypeOpenAICompat, APIKey: "configured",
		Models: []catwalk.Model{
			{ID: "glm-5.3", ReasoningLevels: []string{"high", "max"}, DefaultReasoningEffort: "high"},
			{ID: "glm-new"},
		},
	}
	a.Store().SetProviderRuntimeConfig("zai", provider)
	wire, ok := buildConfigWire(a)
	require.True(t, ok)
	require.Len(t, wire.Providers["zai"].Models, 2)
	require.Empty(t, wire.Providers["zai"].Models[0].ReasoningLevels,
		"static catalog levels must not masquerade as server-reported capabilities")

	provider.LiveEfforts = map[string]config.ModelEffortInfo{
		"glm-5.3": {Levels: []string{"low", "high", "max"}, Default: "max"},
	}
	a.Store().SetProviderRuntimeConfig("zai", provider)
	wire, ok = buildConfigWire(a)
	require.True(t, ok)
	require.Equal(t, []string{"low", "high", "max"}, wire.Providers["zai"].Models[0].ReasoningLevels)
	require.Equal(t, "max", wire.Providers["zai"].Models[0].DefaultReasoningEffort)
	require.Empty(t, wire.Providers["zai"].Models[1].ReasoningLevels)
}
