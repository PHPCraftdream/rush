package provpeak

import (
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

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
		a, err := cmdkit.SetupAppLite(cmd)
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
	_, globalPath, workspacePath, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, probe, policySeedJSON, "", ".rush")
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
