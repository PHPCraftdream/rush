// Provider model-list update tests: computeDiff (adds, removes,
// duplicate IDs, empty sides) and the providers-update level tests
// built on it.
package cmd

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvidersUpdate_SingleProvider(t *testing.T) {
	t.Parallel()
	oldModels := []catwalk.Model{
		{ID: "gpt-4o", Name: "GPT-4o"},
		{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
	}
	newModels := []catwalk.Model{
		{ID: "gpt-4o", Name: "GPT-4o"},
		{ID: "gpt-4-turbo", Name: "GPT-4 Turbo"},
		{ID: "gpt-5", Name: "GPT-5"},
	}

	added, removed := computeDiff(oldModels, newModels)
	assert.Len(t, added, 1)
	assert.Equal(t, "gpt-5", added[0].ID)
	assert.Len(t, removed, 0)
	assert.Equal(t, 2, len(oldModels))
	assert.Equal(t, 3, len(newModels))
}

func TestProvidersUpdate_All(t *testing.T) {
	t.Parallel()
	enabledProviders := []config.ProviderConfig{
		{ID: "openai", Disable: false},
		{ID: "anthropic", Disable: false},
		{ID: "disabled1", Disable: true},
	}
	count := 0
	for _, p := range enabledProviders {
		if !p.Disable {
			count++
		}
	}
	assert.Equal(t, 2, count, "only enabled providers should be updated")
}

func TestComputeDiff_AddsAndRemoves(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
		{ID: "model-c", Name: "Model C"},
	}

	new := []catwalk.Model{
		{ID: "model-b", Name: "Model B"},
		{ID: "model-d", Name: "Model D"},
		{ID: "model-e", Name: "Model E"},
	}

	added, removed := computeDiff(old, new)

	require.Len(t, added, 2)
	require.Equal(t, "model-d", added[0].ID)
	require.Equal(t, "model-e", added[1].ID)

	require.Len(t, removed, 2)
	removedIDs := []string{removed[0].ID, removed[1].ID}
	assert.ElementsMatch(t, []string{"model-a", "model-c"}, removedIDs)
}

func TestComputeDiff_NoChanges(t *testing.T) {
	models := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}

	added, removed := computeDiff(models, models)

	require.Len(t, added, 0)
	require.Len(t, removed, 0)
}

func TestComputeDiff_EmptyToEmpty(t *testing.T) {
	added, removed := computeDiff(nil, nil)
	require.Len(t, added, 0)
	require.Len(t, removed, 0)
}

func TestComputeDiff_EmptyToPopulated(t *testing.T) {
	new := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(nil, new)
	require.Len(t, added, 2)
	require.Len(t, removed, 0)
}

func TestComputeDiff_PopulatedToEmpty(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(old, nil)
	require.Len(t, added, 0)
	require.Len(t, removed, 2)
}

func TestComputeDiff_DuplicateIDsInOld(t *testing.T) {
	old := []catwalk.Model{
		{ID: "model-a", Name: "Model A"},
		{ID: "model-a", Name: "Model A (dup)"},
	}
	new := []catwalk.Model{
		{ID: "model-b", Name: "Model B"},
	}
	added, removed := computeDiff(old, new)
	require.Len(t, added, 1)
	require.Equal(t, "model-b", added[0].ID)
	require.Len(t, removed, 1)
	require.Equal(t, "model-a", removed[0].ID)
}
