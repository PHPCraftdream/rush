package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestModelMergesDoNotReserveDuplicateDiscoveryRows(t *testing.T) {
	t.Parallel()
	configured := []catwalk.Model{{ID: "kept", Name: "Configured", ContextWindow: 100}}
	discovered := make([]catwalk.Model, 64)
	for i := range discovered {
		discovered[i] = catwalk.Model{ID: "kept", Name: "Live", ContextWindow: int64(200 + i)}
	}
	original := configured[0]

	t.Run("codex keeps configured facts without speculative capacity", func(t *testing.T) {
		t.Parallel()
		got := mergeCodexModels(configured, discovered)
		require.Equal(t, configured, got)
		require.Equal(t, len(configured), cap(got), "duplicate live rows must not inflate the initial allocation")
		got[0].Name = "result-only mutation"
		require.Equal(t, original, configured[0], "the merged slice must be owned")
	})
	t.Run("provider overlays facts without speculative capacity", func(t *testing.T) {
		t.Parallel()
		got := mergeProviderModels(configured, discovered)
		require.Equal(t, []catwalk.Model{{ID: "kept", Name: "Live", ContextWindow: 263}}, got)
		require.Equal(t, len(configured), cap(got), "duplicate live rows must not inflate the initial allocation")
		require.Equal(t, original, configured[0], "live overlays must not mutate the configured snapshot")
	})
}

func TestModelMergesPreserveOrderAndGrowForNewIDs(t *testing.T) {
	t.Parallel()
	configured := []catwalk.Model{{ID: "first", Name: "Configured"}}
	live := []catwalk.Model{{ID: "second", Name: "Second"}, {ID: "first", Name: "Live"}, {ID: "third", Name: "Third"}}
	require.Equal(t, []catwalk.Model{
		{ID: "first", Name: "Configured"}, {ID: "second", Name: "Second"}, {ID: "third", Name: "Third"},
	}, mergeCodexModels(configured, live))
	require.Equal(t, []catwalk.Model{
		{ID: "first", Name: "Live"}, {ID: "second", Name: "Second"}, {ID: "third", Name: "Third"},
	}, mergeProviderModels(configured, live))
	require.NotNil(t, mergeCodexModels(nil, nil))
	require.NotNil(t, mergeProviderModels(nil, nil))
}
