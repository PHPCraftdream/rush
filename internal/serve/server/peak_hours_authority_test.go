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
)

func TestPeakPauseResumePreservesSettingsAuthority(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	for _, scope := range []config.Scope{config.ScopeGlobal, config.ScopeWorkspace} {
		for _, operation := range []string{"pause", "resume"} {
			for _, trusted := range []bool{false, true} {
				name := scope.String() + "/" + operation + "/guarded"
				if trusted {
					name = scope.String() + "/" + operation + "/trusted"
				}
				t.Run(name, func(t *testing.T) {
					isolateAllGlobalConfigPaths(t)
					config.SetProcessPassword("")
					t.Cleanup(func() { config.SetProcessPassword("") })
					workspaceData := t.TempDir()
					a := newAttachmentsTestApp(t, t.TempDir(), workspaceData)
					store := a.Store()
					const id = "authority-peak"
					require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers."+id, config.ProviderConfig{
						ID: id, Name: id, Type: "openai-compat", BaseURL: "http://peak.example", APIKey: "fixture",
						Models: []catwalk.Model{{ID: "fixture", Name: "fixture"}},
					}))
					policy := &config.PeakHoursPolicy{
						Timezone: "UTC", Windows: []config.PeakHoursWindowSpec{{Start: "00:00", End: "24:00"}},
						Message: "retained",
					}
					if operation == "resume" {
						policy.PausedUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
					}
					require.NoError(t, store.SetConfigField(scope, "providers."+id+".peak_hours", &config.ProviderPeakHours{Policy: policy}))
					require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
					globalPath := config.GlobalConfigData()
					beforeGlobal, err := os.ReadFile(globalPath)
					require.NoError(t, err)
					lockHash := lockedSyncRev(t, beforeGlobal)
					targetPath := globalPath
					wireScope := "global"
					if scope == config.ScopeWorkspace {
						targetPath = filepath.Join(workspaceData, "rush.json")
						wireScope = "local"
					}
					beforeTarget, err := os.ReadFile(targetPath)
					require.NoError(t, err)
					client := newTestClient()
					client.trustedSettingsWrite = trusted
					payload, err := json.Marshal(map[string]string{"id": id, "scope": wireScope, "duration": "1h"})
					require.NoError(t, err)
					msg := WSMessage{ID: operation, Payload: payload}
					if operation == "pause" {
						handlePauseProviderPeakHours(a, client, msg)
					} else {
						handleResumeProviderPeakHours(a, client, msg)
					}
					reply := readTrustedReply(t, client)
					afterTarget, err := os.ReadFile(targetPath)
					require.NoError(t, err)
					afterGlobal, err := os.ReadFile(globalPath)
					require.NoError(t, err)
					require.Equal(t, lockHash, lockedSyncRev(t, afterGlobal))
					if !trusted {
						require.Equal(t, EventError, reply.Type)
						require.Contains(t, reply.Error, config.ErrSettingsLocked.Error())
						require.Equal(t, beforeTarget, afterTarget)
						require.Equal(t, beforeGlobal, afterGlobal)
						require.Empty(t, pendingConfigEnvelope(t, client.hub))
						return
					}
					require.Equal(t, EventResponse, reply.Type)
					require.Empty(t, reply.Error)
					require.NotEqual(t, beforeTarget, afterTarget)
					entry, err := store.PeakHoursEntryForScope(scope, id)
					require.NoError(t, err)
					require.NotNil(t, entry)
					require.NotNil(t, entry.Policy)
					require.Equal(t, policy.Windows, entry.Policy.Windows)
					require.Equal(t, policy.Timezone, entry.Policy.Timezone)
					require.Equal(t, policy.Message, entry.Policy.Message)
					require.Equal(t, operation == "pause", entry.Status(time.Now()).Paused)
					require.NotEmpty(t, pendingConfigEnvelope(t, client.hub))
					if scope == config.ScopeWorkspace {
						require.Equal(t, beforeGlobal, afterGlobal)
					}
				})
			}
		}
	}
}
