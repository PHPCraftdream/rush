package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPingCmd_CommandsRegistered(t *testing.T) {
	t.Parallel()

	var pingFound, pingFastFound bool
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "ping" {
			pingFound = true
		}
		if cmd.Name() == "ping-fast" {
			pingFastFound = true
		}
	}

	require.True(t, pingFound, "ping command should be registered")
	require.True(t, pingFastFound, "ping-fast command should be registered")
}
