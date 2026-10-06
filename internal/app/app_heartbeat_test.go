package app

// Revert-check map (test -> the single-line production change it must catch):
//   TestAppHeartbeatWiring -> app.New's heartbeat.SetDirFunc+Init block and
//     releaseResources' heartbeat.Shutdown (rows published stopped)
//   TestHeartbeatDirNeverRealInTests -> the testing.Testing() guard in
//     heartbeatDir (tests would write the operator's real registry)

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/stretchr/testify/require"
)

func TestAppHeartbeatWiring(t *testing.T) {
	isolateAppNewTestEnv(t)
	hbDir := t.TempDir()
	t.Setenv("RUSH_HEARTBEAT_DIR", hbDir)
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.ResetForTest)

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "prov", Model: "smart-1"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "prov", Model: "fast-1"})

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store, SkipAgentSetup())
	require.NoError(t, err)

	heartbeat.RecordRequest(heartbeat.WithContext(context.Background(), heartbeat.Context{
		SessionID: "hb-wiring-probe", Purpose: heartbeat.PurposeTurn, Role: "smart", Source: "test",
	}), "prov", "smart-1", nil)

	application.Shutdown()

	entries, err := heartbeat.ReadAll()
	require.NoError(t, err)
	var found *heartbeat.Entry
	for i := range entries {
		if entries[i].Session == "hb-wiring-probe" {
			found = &entries[i]
		}
	}
	require.NotNil(t, found, "heartbeat row for the probe session missing")
	require.Equal(t, "stopped", found.State, "App.Shutdown must publish heartbeat rows stopped")
	require.Equal(t, dataDir, found.Workspace)
	require.Equal(t, "prov/smart-1", found.SlotsAtStart["smart"])
	require.Equal(t, "prov/fast-1", found.SlotsAtStart["fast"])
	require.NotContains(t, found.SlotsAtStart, "worker")
	require.NotContains(t, found.SlotsAtStart, "reviewer")
	require.Equal(t, int64(1), found.Totals.Requests)
	require.NotNil(t, found.Models["prov/smart-1"])
	require.Equal(t, int64(1), found.Models["prov/smart-1"].Requests)
}

func TestHeartbeatDirNeverRealInTests(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", "")
	require.Empty(t, heartbeatDir())
	tmp := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", tmp)
	require.Equal(t, filepath.Join(tmp, "heartbeat"), heartbeatDir())
}
