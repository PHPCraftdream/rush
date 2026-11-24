package run

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/require"
)

func TestRunReportRetryFlagIsWired(t *testing.T) {
	cmd := NewRunCmd()
	flag := cmd.Flags().Lookup("report-retry")
	require.NotNil(t, flag)
	require.Equal(t, "1", flag.DefValue)
	require.NoError(t, cmd.Flags().Parse([]string{"--report-retry", "0"}))
	var overrides app.RunOverrides
	require.NoError(t, applyReportRetryOverride(cmd, &overrides))
	require.Zero(t, overrides.ReportRetries, "the CLI flag value passed into RunOverrides must be zero")
}

// TestRunPermissionFlagsRemoved pins the removal of the rush run permission
// CLI flags: neither registration nor parsing may accept them again. Revert-check:
// if any removed flag is re-registered, Lookup returns a non-nil flag and parsing
// accepts it, so the assertions fail.
func TestRunPermissionFlagsRemoved(t *testing.T) {
	cmd := NewRunCmd()
	for _, name := range []string{"restrict-run", "allow-bash", "allow-tool"} {
		require.Nil(t, cmd.Flags().Lookup(name),
			"rush run flag --%s was removed; re-registering it must fail this test", name)
		err := cmd.Flags().Parse([]string{"--" + name})
		require.Error(t, err, "passing --%s to rush run must be rejected", name)
		require.ErrorContains(t, err, "unknown flag: --"+name,
			"--%s must be rejected as an unknown cobra flag", name)
	}
}

// TestRunHelpOffersConfigNotPermissionFlags pins the run help text: it
// must not advertise the removed permission flags and must still point
// at the permissions.run config path. Revert-check: re-adding flag
// examples or flag descriptions to the help fails the NotContains
// assertions; dropping the config-path documentation from the help
// fails the Contains assertions.
func TestRunHelpOffersConfigNotPermissionFlags(t *testing.T) {
	t.Parallel()

	long := NewRunCmd().Long
	require.NotContains(t, long, "--restrict-run")
	require.NotContains(t, long, "--allow-bash")
	require.NotContains(t, long, "--allow-tool")
	require.Contains(t, long, "permissions.run")
	require.Contains(t, long, "allow_bash")
	require.Contains(t, long, "allow_tools")
}
