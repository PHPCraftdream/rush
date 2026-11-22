package clie2etest

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestSessionsCostRejectsPositionalArgs(t *testing.T) {
	root := cli.NewRootCmd()
	sessionsCmd, _, err := root.Find([]string{"sessions"})
	require.NoError(t, err)
	var sessionsCostCmd *cobra.Command
	for _, sub := range sessionsCmd.Commands() {
		if sub.Name() == "cost" {
			sessionsCostCmd = sub
			break
		}
	}
	require.NotNil(t, sessionsCostCmd)
	require.Error(t, sessionsCostCmd.Args(sessionsCostCmd, []string{"some-session"}))
	require.NoError(t, sessionsCostCmd.Args(sessionsCostCmd, nil))
}
