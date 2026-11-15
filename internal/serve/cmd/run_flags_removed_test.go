package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunPermissionFlagsRemoved pins the removal of the rush run
// permission CLI flags: neither registration nor parsing may accept
// them again. Revert-check: if any of the three flags is re-registered
// on runCmd (runCmd.Flags().Bool/StringSlice in run.go's init), Lookup
// returns a non-nil flag and Parse succeeds, so both assertions fail.
func TestRunPermissionFlagsRemoved(t *testing.T) {
	for _, name := range []string{"restrict-run", "allow-bash", "allow-tool"} {
		require.Nil(t, runCmd.Flags().Lookup(name),
			"rush run flag --%s was removed; re-registering it must fail this test", name)
		err := runCmd.Flags().Parse([]string{"--" + name})
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

	long := runCmd.Long
	require.NotContains(t, long, "--restrict-run")
	require.NotContains(t, long, "--allow-bash")
	require.NotContains(t, long, "--allow-tool")
	require.Contains(t, long, "permissions.run")
	require.Contains(t, long, "allow_bash")
	require.Contains(t, long, "allow_tools")
}
