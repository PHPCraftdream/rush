package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestSessionsCostRejectsPositionalArgs(t *testing.T) {
	cost := NewSessionsCmd().Commands()
	var sessionsCostCmd *cobra.Command
	for _, sub := range cost {
		if sub.Name() == "cost" {
			sessionsCostCmd = sub
			break
		}
	}
	require.NotNil(t, sessionsCostCmd)
	require.Error(t, sessionsCostCmd.Args(sessionsCostCmd, []string{"some-session"}))
	require.NoError(t, sessionsCostCmd.Args(sessionsCostCmd, nil))
}
