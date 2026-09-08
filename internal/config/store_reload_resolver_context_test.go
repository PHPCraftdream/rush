package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReloadFromDisk_PublishedResolverOutlivesReloadContext(t *testing.T) {
	root := t.TempDir()
	isolateAllGlobalConfigPaths(t)
	t.Setenv("RUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(root, "rush.json")
	writeResolverContextConfig(t, configPath, `"$(printf runtime-token)"`)

	store, err := Load(root, root, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, store.ReloadFromDisk(ctx))
	cancel()

	mcp, ok := store.MCPConfig("runtime")
	require.True(t, ok)
	headers, err := mcp.ResolvedHeaders(store.Resolver())
	require.NoError(t, err)
	require.Equal(t, "runtime-token", headers["X-Token"])
}

func TestReloadFromDisk_CancelAbortsCandidateSubstitution(t *testing.T) {
	root := t.TempDir()
	isolateAllGlobalConfigPaths(t)
	t.Setenv("RUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(root, "rush.json")
	writeResolverContextConfig(t, configPath, `"static-token"`)

	store, err := Load(root, root, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})
	before := store.Config()

	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"options": {"disable_default_providers": true},
		"providers": {
			"custom": {
				"api_key": "static-token",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": "model"}],
				"extra_headers": {"X-Block": "$(candidate-value)"}
			}
		}
	}`), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	entered := make(chan struct{})
	var enteredOnce sync.Once
	store.reloadResolverExpander = func(expandCtx context.Context, value string, _ []string) (string, error) {
		if value != "$(candidate-value)" {
			return value, nil
		}
		enteredOnce.Do(func() { close(entered) })
		<-expandCtx.Done()
		return "", expandCtx.Err()
	}

	done := make(chan error, 1)
	go func() {
		done <- store.ReloadFromDisk(ctx)
	}()
	<-entered
	cancel()

	err = <-done
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "reload must retain cancellation cause: %v", err)
	require.Same(t, before, store.Config(), "cancelled candidate must not be published")
}

func writeResolverContextConfig(t *testing.T, path, token string) {
	t.Helper()
	content := `{
		"options": {"disable_default_providers": true},
		"providers": {
			"custom": {
				"api_key": "static-token",
				"base_url": "https://example.invalid/v1",
				"models": [{"id": "model"}]
			}
		},
		"mcp": {
			"runtime": {
				"type": "http",
				"url": "https://example.invalid/mcp",
				"headers": {"X-Token": ` + token + `}
			}
		}
	}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
