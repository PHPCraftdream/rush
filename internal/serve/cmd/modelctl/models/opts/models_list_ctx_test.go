package modelopts

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/stretchr/testify/require"
)

func TestAtomContextParityWithOtherModels(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	for _, id := range []string{"local-cli", "zai"} {
		cfg.Providers.Set(id, config.ProviderConfig{ID: id})
	}
	keys := modelatoms.EnabledAtomKeys(cfg)
	require.ElementsMatch(t, []string{
		"opus", "opus46", "opus47", "opus48", "sonnet", "haiku", "fable", "opus5", "sonnet5", "fable5",
		"glm5_3", "glm5_turbo", "glm5_3_flash", "glm4_7", "glm4_7_flash", "glm4_6", "glm4_6v",
	}, keys)

	atoms := make(map[string]modelatoms.Atom, len(keys))
	for _, key := range keys {
		a, ok := modelatoms.LookupAtom(key)
		require.True(t, ok, key)
		atoms[key] = a
		cw := int64(1_000_000)
		if a.Provider == "zai" {
			cw = 200_000
		}
		if key == "glm5_3_flash" {
			// The documented 1 Mi-token window distinguishes 1.05M from 1M or 1.0M.
			cw = 1_048_576
		}
		p, ok := cfg.Providers.Get(a.Provider)
		require.True(t, ok, key)
		p.Models = append(p.Models, catwalk.Model{ID: a.Model, ContextWindow: cw})
		cfg.Providers.Set(a.Provider, p)
	}

	for _, key := range keys {
		a := atoms[key]
		m := cfg.GetModel(a.Provider, a.Model)
		require.NotNil(t, m, key)
		otherCtx := modelListCtx(m.ContextWindow)
		atomCtx := modelatoms.AtomCtx(cfg, a)
		require.Equal(t, otherCtx, atomCtx, key)
		if key == "glm5_3_flash" {
			require.Equal(t, "1.05M", otherCtx, key)
		}
	}
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
		require.Equal(t, test.want, modelatoms.HumanCtx(test.in), "modelatoms.HumanCtx(%d)", test.in)
	}
}
