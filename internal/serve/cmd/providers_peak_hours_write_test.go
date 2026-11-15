package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWritePeakEntryWorkspaceOffDoesNotReinheritGlobal(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	var effectiveOff bool
	probe := &cobra.Command{Use: "peak-off-probe", RunE: func(cmd *cobra.Command, _ []string) error {
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		local := &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{
			Windows: []config.PeakHoursWindowSpec{{Start: "09:00", End: "10:00"}},
		}}
		if err := a.Store().SetConfigField(config.ScopeWorkspace, peakFieldPath("polprov"), local); err != nil {
			return err
		}
		// Removing the last window produces this metadata-free off value.
		if err := writePeakEntry(a, config.ScopeWorkspace, "polprov", &config.ProviderPeakHours{Policy: &config.PeakHoursPolicy{}}); err != nil {
			return err
		}
		provider, exists := a.Store().Config().Providers.Get("polprov")
		effectiveOff = exists && (provider.PeakHours == nil || provider.PeakHours.IsOff())
		return nil
	}}
	_, globalPath, workspacePath, err := runProvidersCmdInIsolatedAppFull(t, probe, policySeedJSON, "", ".rush")
	require.NoError(t, err)
	require.NotEmpty(t, workspacePath)
	require.NotEqual(t, globalPath, workspacePath)
	require.True(t, effectiveOff, "the reloaded workspace off must suppress the global schedule")
	localBytes, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	require.Equal(t, "null", gjson.GetBytes(localBytes, "providers.polprov.peak_hours").Raw,
		"off must remain an explicit key, not inheritance")
	globalBytes, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.JSONEq(t, gjson.Get(policySeedJSON, "providers.polprov.peak_hours").Raw,
		gjson.GetBytes(globalBytes, "providers.polprov.peak_hours").Raw)
}

func TestLegacyPeakWindowEditPreservesWeeklyMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   string
		message string
	}{
		{"keep message", "", "weekly-msg"},
		{"replace message", " --peak-hours-message replacement", "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := config.SetKtavAutoMigrateForTest(false)
			t.Cleanup(restore)
			const until = "2026-11-09T18:00:00Z"
			seed := strings.Replace(policySeedJSON, `"peak_hours": {`, `"peak_hours": {"timezone":"UTC+08:00","paused_until":"`+until+`",`, 1)
			_, path, _, err := runProvidersCmdInIsolatedAppFull(t, providersSetCmd, seed, "polprov --peak-hours 09:00-17:00"+tc.flags, "")
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var entry config.ProviderPeakHours
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(data, "providers.polprov.peak_hours").Raw), &entry))
			require.NotNil(t, entry.Policy)
			require.Nil(t, entry.Legacy)
			require.Equal(t, "UTC+08:00", entry.Policy.Timezone)
			require.Equal(t, until, entry.Policy.PausedUntil)
			require.Equal(t, tc.message, entry.Policy.Message)
			require.Equal(t, []config.PeakHoursWindowSpec{{Start: "09:00", End: "17:00"}}, entry.Policy.Windows)
		})
	}
}
