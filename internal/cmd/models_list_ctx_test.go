package cmd

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/require"
)

func TestAtomContextParityWithOtherModels(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	providers := map[string][]catwalk.Model{}
	for _, a := range atomRegistry {
		cw := int64(1_000_000)
		if a.Provider == "zai" {
			cw = 200_000
		}
		providers[a.Provider] = append(providers[a.Provider], catwalk.Model{ID: a.Model, ContextWindow: cw})
	}
	for id, models := range providers {
		cfg.Providers.Set(id, config.ProviderConfig{ID: id, Models: models})
	}
	for key, a := range atomRegistry {
		p, ok := cfg.Providers.Get(a.Provider)
		require.True(t, ok)
		var matched *catwalk.Model
		for i := range p.Models {
			if p.Models[i].ID == a.Model {
				matched = &p.Models[i]
				break
			}
		}
		require.NotNil(t, matched, key)
		require.Equal(t, modelListCtx(matched.ContextWindow), atomCtx(cfg, a), key)
	}
}

func TestAtomContextUsesRuntimeModelThenFacts(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	a := atomRegistry["glm5_3"]
	cw, _ := discover.LookupModelFacts("zai", "glm-5.3")
	cfg.Providers.Set("zai", config.ProviderConfig{ID: "zai", Models: []catwalk.Model{{ID: "glm-5.3", ContextWindow: 333_000}}})
	require.Equal(t, "333k", atomCtx(cfg, a))
	// Revert-check: fixed GLM-5.3 labeling would disagree with runtime data.
	require.NotEqual(t, humanCtx(cw), atomCtx(cfg, a))
	cfg.Providers.Set("zai", config.ProviderConfig{ID: "zai", Models: []catwalk.Model{{ID: "glm-5.3"}}})
	require.Equal(t, humanCtx(cw), atomCtx(cfg, a))
}

func TestOtherModelsContextUnknownIsQuestionMark(t *testing.T) {
	require.Equal(t, "?", modelListCtx(0))
	require.Equal(t, "?", modelListCtx(-1))
	require.Equal(t, "1M", modelListCtx(1_000_000))
}

// TestHumanCtx pins the compact context-window rendering of `rush models
// list`: 1 050 000 must read 1.05M (it read a rounded 1.1M, which looks like a
// different window than the documented one), a round million stays "1M".
//
// Revert-check: format millions with %.1f again and the 1050000 row reads 1.1M.
func TestHumanCtx(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{1_050_000, "1.05M"},
		{1_000_000, "1M"},
		{1_500_000, "1.5M"},
		{2_000_000, "2M"},
		{1_048_576, "1.05M"},
		{272_000, "272k"},
		{999, "999"},
	}
	for _, test := range tests {
		require.Equal(t, test.want, humanCtx(test.in), "humanCtx(%d)", test.in)
	}
}
