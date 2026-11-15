package server

// Task #1061: the web model selector generalized from smart/fast-only to
// all four role slots, including each role's own "recent models" list.
// RecordRecentModel (internal/config/store_models.go) already accepted any
// config.SelectedModelType, worker/reviewer included — buildConfigWire just
// never read cfg.RecentModels[Worker]/[Reviewer] back into the wire struct.
// This test pins that the wire now carries all four lists.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildConfigWire_SurfacesRecentModelsForAllFourRoles(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())

	store := a.Store()
	require.NotNil(t, store)

	require.NoError(t, store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeSmart,
		config.SelectedModel{Provider: "smart-provider", Model: "smart-model"}))
	require.NoError(t, store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeFast,
		config.SelectedModel{Provider: "fast-provider", Model: "fast-model"}))
	require.NoError(t, store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeWorker,
		config.SelectedModel{Provider: "worker-provider", Model: "worker-model"}))
	require.NoError(t, store.RecordRecentModel(config.ScopeGlobal, config.SelectedModelTypeReviewer,
		config.SelectedModel{Provider: "reviewer-provider", Model: "reviewer-model"}))

	wire, ok := buildConfigWire(a)
	require.True(t, ok)

	require.Equal(t, []ModelEntryWire{{Provider: "smart-provider", Model: "smart-model"}}, wire.RecentSmartModels)
	require.Equal(t, []ModelEntryWire{{Provider: "fast-provider", Model: "fast-model"}}, wire.RecentFastModels)
	require.Equal(t, []ModelEntryWire{{Provider: "worker-provider", Model: "worker-model"}}, wire.RecentWorkerModels,
		"worker recents must reach the wire, not just the config file")
	require.Equal(t, []ModelEntryWire{{Provider: "reviewer-provider", Model: "reviewer-model"}}, wire.RecentReviewerModels,
		"reviewer recents must reach the wire, not just the config file")
}
