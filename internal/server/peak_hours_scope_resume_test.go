package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResumeGlobalPeakPausePreservesEffectiveWorkspacePolicy(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	const id = "peak-scope"
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers."+id, config.ProviderConfig{
		ID: id, Name: id, Type: "openai-compat", BaseURL: "http://peak.example", APIKey: "fixture",
		Models: []catwalk.Model{{ID: "fixture", Name: "fixture"}},
	}))
	global := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		PausedUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	local := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC",
		Windows:  []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
		Message:  "workspace policy",
	}}
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers."+id+".peak_hours", global))
	require.NoError(t, store.SetConfigField(config.ScopeWorkspace, "providers."+id+".peak_hours", local))
	before, ok := store.Config().Providers.Get(id)
	require.True(t, ok)
	require.Equal(t, local, before.PeakHours)

	client := newTestClient()
	payload, err := json.Marshal(ResumeProviderPeakHoursPayload{ID: id, Scope: "global"})
	require.NoError(t, err)
	handleResumeProviderPeakHours(a, client, WSMessage{ID: "resume-global", Payload: payload})
	select {
	case raw := <-client.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(raw, &reply))
		require.Equal(t, EventResponse, reply.Type)
		require.Empty(t, reply.Error)
	default:
		t.Fatal("resume handler did not reply")
	}

	current, ok := store.Config().Providers.Get(id)
	require.True(t, ok)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("global resume: provider_present=%v effective_peak_present=%v", ok, current.PeakHours != nil)
		}
	})
	require.Equal(t, local, current.PeakHours, "scoped removal must not overwrite the reloaded effective policy")
	require.True(t, current.PeakHours.Status(time.Now()).Blocked)
	globalEntry, err := store.PeakHoursEntryForScope(config.ScopeGlobal, id)
	require.NoError(t, err)
	require.Nil(t, globalEntry)
	localEntry, err := store.PeakHoursEntryForScope(config.ScopeWorkspace, id)
	require.NoError(t, err)
	require.Equal(t, local, localEntry)

	client.hub.configMu.Lock()
	pending := append([]byte(nil), client.hub.configPending...)
	client.hub.configMu.Unlock()
	var broadcast WSMessage
	require.NoError(t, json.Unmarshal(pending, &broadcast))
	require.Equal(t, EventConfig, broadcast.Type)
	require.Equal(t, "workspace policy", gjson.GetBytes(broadcast.Payload, "providers."+id+".peakHours.message").String())
}

func TestResumeWorkspacePauseOnlyKeepsExplicitOff(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	workspaceData := t.TempDir()
	a := newAttachmentsTestApp(t, t.TempDir(), workspaceData)
	store := a.Store()
	const id = "resume-local-off"
	global := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
		Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
	}}
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers."+id, config.ProviderConfig{
		ID: id, Name: id, Type: "openai-compat", BaseURL: "http://peak.example", APIKey: "fixture",
		Models: []catwalk.Model{{ID: "fixture", Name: "fixture"}}, PeakHours: global,
	}))
	require.NoError(t, store.SetConfigField(config.ScopeWorkspace, "providers."+id+".peak_hours",
		&config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			PausedUntil: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}}))
	before, ok := store.Config().Providers.Get(id)
	require.True(t, ok)
	require.NotNil(t, before.PeakHours)
	require.True(t, before.PeakHours.IsOff())

	client := newTestClient()
	payload, err := json.Marshal(ResumeProviderPeakHoursPayload{ID: id, Scope: "local"})
	require.NoError(t, err)
	handleResumeProviderPeakHours(a, client, WSMessage{ID: "resume-local", Payload: payload})
	select {
	case raw := <-client.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(raw, &reply))
		require.Equal(t, EventResponse, reply.Type)
		require.Empty(t, reply.Error)
	default:
		t.Fatal("resume handler did not reply")
	}
	localBytes, err := os.ReadFile(filepath.Join(workspaceData, "rush.json"))
	require.NoError(t, err)
	require.Equal(t, "null", gjson.GetBytes(localBytes, "providers."+id+".peak_hours").Raw)
	current, ok := store.Config().Providers.Get(id)
	require.True(t, ok)
	require.Nil(t, current.PeakHours, "resume must not restore the inherited full-week prohibition")
	globalEntry, err := store.PeakHoursEntryForScope(config.ScopeGlobal, id)
	require.NoError(t, err)
	require.Equal(t, global, globalEntry)
}

func TestCustomProviderWorkspaceOffKeepsExplicitNull(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	workspaceData := t.TempDir()
	a := newAttachmentsTestApp(t, t.TempDir(), workspaceData)
	store := a.Store()
	const id = "custom-peak-off"
	global := config.ProviderConfig{
		ID: id, Name: id, Type: "openai-compat", BaseURL: "http://peak.example", APIKey: "fixture",
		Models: []catwalk.Model{{ID: "fixture", Name: "fixture"}},
		PeakHours: &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
		}},
	}
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers."+id, global))
	payload, err := json.Marshal(UpdateCustomProviderPayload{
		OldID: id, ID: id, Name: id, Type: "openai-compat", BaseURL: "http://peak.example",
		Models: []CustomModelPayload{{ID: "fixture", Name: "fixture"}}, Scope: "local", PeakHours: nil,
	})
	require.NoError(t, err)
	client := newTestClient()
	handleUpdateCustomProvider(a, client, WSMessage{ID: "custom-off", Payload: payload})
	select {
	case raw := <-client.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(raw, &reply))
		require.Equal(t, EventResponse, reply.Type)
		require.Empty(t, reply.Error)
	default:
		t.Fatal("custom provider update did not reply")
	}
	localBytes, err := os.ReadFile(filepath.Join(workspaceData, "rush.json"))
	require.NoError(t, err)
	require.Equal(t, "null", gjson.GetBytes(localBytes, "providers."+id+".peak_hours").Raw)
	current, exists := store.Config().Providers.Get(id)
	require.True(t, exists)
	require.True(t, current.PeakHours == nil || current.PeakHours.IsOff(), "local off must suppress the global schedule after reload")
	globalEntry, err := store.PeakHoursEntryForScope(config.ScopeGlobal, id)
	require.NoError(t, err)
	require.Equal(t, global.PeakHours, globalEntry)
}
