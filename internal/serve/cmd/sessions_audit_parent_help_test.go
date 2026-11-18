package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionsParentHelp_ListsAudit: the `sessions` root help lists every
// subcommand it has — one that is missing from it does not exist for the
// operator reading it.
//
// Revert-check: removing "audit" from the Observe block (or from
// AddCommand) turns this red.
func TestSessionsParentHelp_ListsAudit(t *testing.T) {
	t.Parallel()

	observe := oneLine(sessionsCmd.Long)
	require.Contains(t, observe, "audit")
	require.Contains(t, observe, "anomaly scan")

	var registered bool
	for _, sub := range sessionsCmd.Commands() {
		if sub.Name() == "audit" {
			registered = true
			require.Same(t, sessionsAuditCmd, sub, "the registered command must be the one under test")
		}
	}
	require.True(t, registered, "`sessions audit` must be registered on the sessions command")
}
