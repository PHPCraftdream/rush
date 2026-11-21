package modelatoms

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/discover"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/stretchr/testify/require"
)

func TestAtomContextUsesRuntimeModelThenFacts(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
	a, ok := LookupAtom("glm5_3")
	require.True(t, ok)
	cw, _ := discover.LookupModelFacts("zai", "glm-5.3")
	cfg.Providers.Set("zai", config.ProviderConfig{ID: "zai", Models: []catwalk.Model{{ID: "glm-5.3", ContextWindow: 333_000}}})
	require.Equal(t, "333k", AtomCtx(cfg, a))
	// Revert-check: fixed GLM-5.3 labeling would disagree with runtime data.
	require.NotEqual(t, HumanCtx(cw), AtomCtx(cfg, a))
	cfg.Providers.Set("zai", config.ProviderConfig{ID: "zai", Models: []catwalk.Model{{ID: "glm-5.3"}}})
	require.Equal(t, HumanCtx(cw), AtomCtx(cfg, a))
}
