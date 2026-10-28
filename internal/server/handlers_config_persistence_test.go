package server

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSkillsPathPersistenceFailuresDoNotMutateMemory(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*app.App, *Client)
	}{
		{"add", func(a *app.App, c *Client) {
			p, _ := json.Marshal(AddSkillsPathPayload{Path: "extra-skills"})
			handleAddSkillsPath(a, c, WSMessage{ID: "skills", Payload: p})
		}},
		{"remove", func(a *app.App, c *Client) {
			p, _ := json.Marshal(RemoveSkillsPathPayload{Path: "kept-skills"})
			handleRemoveSkillsPath(a, c, WSMessage{ID: "skills", Payload: p})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
			store := a.Store()
			paths := []string{"kept-skills"}
			require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.skills_paths", paths))
			paths = append([]string(nil), store.Config().Options.SkillsPaths...)
			blockConfigWrites(t, store)
			c := newTestClient()
			tc.run(a, c)
			reply := readTestReply(t, c)
			require.Equal(t, EventError, reply.Type)
			require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
			require.Equal(t, paths, store.Config().Options.SkillsPaths)
			assertNoConfigBroadcast(t, c)
		})
	}
}

func TestCustomProviderPersistenceFailuresDoNotMutateMemory(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		store := a.Store()
		blockConfigWrites(t, store)
		payload, err := json.Marshal(AddCustomProviderPayload{ID: "failed-custom", BaseURL: "https://example.invalid/v1", Type: "openai-compat", Models: []CustomModelPayload{{ID: "m", Name: "Model"}}})
		require.NoError(t, err)
		c := newTestClient()
		handleAddCustomProvider(a, c, WSMessage{ID: "provider", Payload: payload})
		reply := readTestReply(t, c)
		require.Equal(t, EventError, reply.Type)
		require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
		_, exists := store.Config().Providers.Get("failed-custom")
		require.False(t, exists)
		assertNoConfigBroadcast(t, c)
	})

	t.Run("rename rejects existing target without overwriting it", func(t *testing.T) {
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		store := a.Store()
		for _, provider := range []config.ProviderConfig{
			{ID: "before", Name: "Before", BaseURL: "https://before.example.invalid/v1"},
			{ID: "after", Name: "Existing", BaseURL: "https://existing.example.invalid/v1"},
		} {
			payload, err := json.Marshal(AddCustomProviderPayload{ID: provider.ID, Name: provider.Name, BaseURL: provider.BaseURL, Type: "openai-compat", Models: []CustomModelPayload{{ID: "m", Name: "Model"}}})
			require.NoError(t, err)
			seedClient := newTestClient()
			handleAddCustomProvider(a, seedClient, WSMessage{ID: "seed", Payload: payload})
			require.Equal(t, EventResponse, readTestReply(t, seedClient).Type)
		}

		payload, err := json.Marshal(UpdateCustomProviderPayload{OldID: "before", ID: "after", Name: "Replacement", BaseURL: "https://replacement.example.invalid/v1", Type: "openai-compat"})
		require.NoError(t, err)
		c := newTestClient()
		handleUpdateCustomProvider(a, c, WSMessage{ID: "rename", Payload: payload})

		reply := readTestReply(t, c)
		require.Equal(t, EventError, reply.Type)
		require.Contains(t, reply.Error, `provider "after" already exists`)
		before, exists := store.Config().Providers.Get("before")
		require.True(t, exists)
		require.Equal(t, "Before", before.Name)
		after, exists := store.Config().Providers.Get("after")
		require.True(t, exists)
		require.Equal(t, "Existing", after.Name)
		assertNoConfigBroadcast(t, c)
	})

	t.Run("update rename preserves old key", func(t *testing.T) {
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		store := a.Store()
		oldProvider := config.ProviderConfig{ID: "before", Name: "Before", BaseURL: "https://before.example.invalid/v1"}
		seed, err := json.Marshal(AddCustomProviderPayload{ID: oldProvider.ID, Name: oldProvider.Name, BaseURL: oldProvider.BaseURL, Type: "openai-compat", Models: []CustomModelPayload{{ID: "m", Name: "Model"}}})
		require.NoError(t, err)
		seedClient := newTestClient()
		handleAddCustomProvider(a, seedClient, WSMessage{ID: "seed", Payload: seed})
		require.Equal(t, EventResponse, readTestReply(t, seedClient).Type)
		oldProvider, exists := store.Config().Providers.Get("before")
		require.True(t, exists)
		blockConfigWrites(t, store)
		payload, err := json.Marshal(UpdateCustomProviderPayload{OldID: "before", ID: "after", Name: "After", BaseURL: "https://after.example.invalid/v1", Type: "openai-compat"})
		require.NoError(t, err)
		c := newTestClient()
		handleUpdateCustomProvider(a, c, WSMessage{ID: "provider", Payload: payload})
		reply := readTestReply(t, c)
		require.Equal(t, EventError, reply.Type)
		require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
		got, exists := store.Config().Providers.Get("before")
		require.True(t, exists, "failed rename must preserve the old provider key")
		require.Equal(t, oldProvider.ID, got.ID)
		require.Equal(t, oldProvider.Name, got.Name)
		_, exists = store.Config().Providers.Get("after")
		require.False(t, exists, "failed rename must not add the new provider key")
		assertNoConfigBroadcast(t, c)
	})

	t.Run("remove preserves provider", func(t *testing.T) {
		a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
		store := a.Store()
		provider := config.ProviderConfig{ID: "kept", Name: "Kept", BaseURL: "https://kept.example.invalid/v1"}
		seed, err := json.Marshal(AddCustomProviderPayload{ID: provider.ID, Name: provider.Name, BaseURL: provider.BaseURL, Type: "openai-compat", Models: []CustomModelPayload{{ID: "m", Name: "Model"}}})
		require.NoError(t, err)
		seedClient := newTestClient()
		handleAddCustomProvider(a, seedClient, WSMessage{ID: "seed", Payload: seed})
		require.Equal(t, EventResponse, readTestReply(t, seedClient).Type)
		provider, exists := store.Config().Providers.Get("kept")
		require.True(t, exists)
		blockConfigWrites(t, store)
		payload, err := json.Marshal(RemoveCustomProviderPayload{ID: "kept", Scope: "global"})
		require.NoError(t, err)
		c := newTestClient()
		handleRemoveCustomProvider(a, c, WSMessage{ID: "provider", Payload: payload})
		reply := readTestReply(t, c)
		require.Equal(t, EventError, reply.Type)
		require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
		got, exists := store.Config().Providers.Get("kept")
		require.True(t, exists, "failed removal must preserve the provider")
		require.Equal(t, provider.ID, got.ID)
		require.Equal(t, provider.Name, got.Name)
		assertNoConfigBroadcast(t, c)
	})
}

func TestRemoveCustomProviderWithoutOverrideSucceeds(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.debug", true))
	c := newTestClient()
	payload, err := json.Marshal(RemoveCustomProviderPayload{ID: "local-cli", Scope: "global"})
	require.NoError(t, err)

	handleRemoveCustomProvider(a, c, WSMessage{ID: "remove", Payload: payload})

	reply := readTestReply(t, c)
	require.Equal(t, EventResponse, reply.Type)
	require.Empty(t, reply.Error)
	_, exists := store.Config().Providers.Get("local-cli")
	require.False(t, exists, "successful removal must delete the provider from the live config")
	select {
	case <-c.hub.configBroadcast:
	default:
		t.Fatal("expected config broadcast after successful removal")
	}
	select {
	case raw := <-c.hub.broadcast:
		t.Fatalf("unexpected broadcast: %s", raw)
	default:
	}
}

func assertNoConfigBroadcast(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.hub.configBroadcast:
		t.Fatal("unexpected config broadcast")
	default:
	}
	select {
	case raw := <-c.hub.broadcast:
		t.Fatalf("unexpected broadcast: %s", raw)
	default:
	}
}

func blockConfigWrites(t *testing.T, store *config.ConfigStore) {
	t.Helper()
	require.NoError(t, store.LockSettings(config.ScopeGlobal, "test lock"))
}

func readTestReply(t *testing.T, c *Client) WSMessage {
	t.Helper()
	select {
	case raw := <-c.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(raw, &reply))
		return reply
	default:
		t.Fatal("handler did not reply")
		return WSMessage{}
	}
}
