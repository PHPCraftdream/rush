package cmd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyModelsListRefreshMode_Default verifies that the default
// `rush models list` path (no --refresh) opts into cache-only mode so
// the provider syncers do not perform a network fetch or write the
// cache. This is the contract that makes `rush models list` side
// effect-free by default.
func TestApplyModelsListRefreshMode_Default(t *testing.T) {
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "")
	t.Setenv("RUSH_PROVIDER_CACHE_TTL", "24h")

	applyModelsListRefreshMode(false)

	assert.Equal(t, "1", os.Getenv("RUSH_PROVIDER_CACHE_ONLY"),
		"default mode must set RUSH_PROVIDER_CACHE_ONLY=1")
	assert.Equal(t, "24h", os.Getenv("RUSH_PROVIDER_CACHE_TTL"),
		"default mode must not perturb RUSH_PROVIDER_CACHE_TTL (cache-only wins anyway)")
}

// TestApplyModelsListRefreshMode_Refresh verifies that `--refresh`
// clears cache-only mode and forces TTL=0 so the syncers always perform
// a network fetch and update the cache, even if the caller's
// environment had previously opted into cache-only mode.
func TestApplyModelsListRefreshMode_Refresh(t *testing.T) {
	// Simulate a user shell that pre-set cache-only mode.
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	t.Setenv("RUSH_PROVIDER_CACHE_TTL", "24h")

	applyModelsListRefreshMode(true)

	assert.Equal(t, "", os.Getenv("RUSH_PROVIDER_CACHE_ONLY"),
		"refresh mode must clear RUSH_PROVIDER_CACHE_ONLY so the syncer hits the network")
	assert.Equal(t, "0", os.Getenv("RUSH_PROVIDER_CACHE_TTL"),
		"refresh mode must force RUSH_PROVIDER_CACHE_TTL=0 so a fresh cache is treated as stale")
}

// TestModelsListCmd_RefreshFlagRegistered verifies the --refresh flag is
// wired onto the command and defaults to off (preserving the new default
// of cache-only/listing without side effects).
func TestModelsListCmd_RefreshFlagRegistered(t *testing.T) {
	flag := modelsListCmd.Flags().Lookup("refresh")
	require.NotNil(t, flag, "--refresh flag must be registered on models list")
	assert.Equal(t, "false", flag.DefValue, "--refresh must default to off")
	assert.Equal(t, "bool", flag.Value.Type(), "--refresh must be a bool flag")
}
