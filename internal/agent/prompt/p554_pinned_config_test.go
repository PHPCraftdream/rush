package prompt

import (
	"context"
	"maps"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// snapshotConfig makes a copy of cfg that is independent of it for the fields
// this test mutates.
//
// A plain *cfg is NOT enough, and getting that wrong is how the first attempt
// at this test came out vacuous: config.ConfigStore.Config() returns a SHARED
// pointer, and Models is a map, so a shallow copy still aliases it. The
// "pinned" and "live" configs were then the same object and the test compared
// a value with itself -- it passed with the fix removed.
//
// Providers is deliberately left aliased: both generations' models are
// registered there, and what distinguishes them is only which one Models
// points at. That is exactly the shape of a real reload that changes a slot.
func snapshotConfig(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.Models = maps.Clone(cfg.Models)
	return &clone
}

// TestBuild_UsesThePinnedConfigNotTheLiveStore covers P1-2 of the 2026-08-18
// release-readiness review (task #554).
//
// resolveSessionModels pins one config generation and resolves the model
// against it, then built the prompt from the LIVE store. A reload landing
// between the two produced a model from generation N and a prompt from N+1.
//
// The probe is WorkerContextWindowText, because it is derived purely from
// cfg.Models plus cfg.GetModel -- no filesystem path feeds it. That matters:
// the obvious probe (Options.ContextPaths) is useless here, because context
// files are ALSO discovered from the working directory, so a marker file
// renders regardless of which generation listed it. Verified directly on the
// first attempt, where both generations' markers appeared.
//
// Revert-check: make promptData read store.Config() again instead of the cfg
// parameter and this fails -- the render reports the live generation's worker
// context window.
func TestBuild_UsesThePinnedConfigNotTheLiveStore(t *testing.T) {
	store := testConfigStore(t)
	live := store.Config()

	// Both worker models exist in the provider catalogue; the generations
	// differ only in which one the Worker slot selects.
	genNWorker := registerProvider(live, "worker-provider-n", "worker-model-n", 200_000)
	genN1Worker := registerProvider(live, "worker-provider-n1", "worker-model-n1", 900_000)

	// Generation N, pinned by the caller exactly as resolveSessionModels
	// pins the config it resolves models against.
	live.Models[config.SelectedModelTypeWorker] = genNWorker
	pinned := snapshotConfig(live)

	// Generation N+1 lands while the caller is still mid-construction.
	live.Models[config.SelectedModelTypeWorker] = genN1Worker

	p := newTestCoderPrompt(t, store.WorkingDir())
	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, pinned, true)
	require.NoError(t, err)

	require.Contains(t, got, "200k tokens",
		"the pinned generation's worker model was not rendered at all, so the assertion below would prove nothing")
	require.NotContains(t, got, "900k tokens",
		"Build read the live store instead of the pinned config: the prompt describes a different generation than the model resolved alongside it")
}

// TestBuild_PinnedConfigIsHonouredWhenTheLiveStoreHasNoWorker is the other
// direction, and the one that rules out a Build which simply ignores the
// parameter. Here the LIVE config has no worker at all; only the pinned
// generation does. The orchestrator block must still be rendered from the
// pinned value.
func TestBuild_PinnedConfigIsHonouredWhenTheLiveStoreHasNoWorker(t *testing.T) {
	store := testConfigStore(t)
	live := store.Config()

	worker := registerProvider(live, "worker-provider-pinned", "worker-model-pinned", 300_000)
	live.Models[config.SelectedModelTypeWorker] = worker
	pinned := snapshotConfig(live)

	// The reload removes the worker slot entirely.
	delete(live.Models, config.SelectedModelTypeWorker)

	p := newTestCoderPrompt(t, store.WorkingDir())
	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, pinned, true)
	require.NoError(t, err)

	require.Contains(t, got, "300k tokens",
		"the pinned generation's worker was dropped because Build consulted the live store, which no longer has one")
}
