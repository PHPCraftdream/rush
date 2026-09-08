//go:build windows

package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsMutatingUtilitiesAreNotSafe(t *testing.T) {
	for _, command := range []string{
		"ipconfig /release",
		"ipconfig /renew",
		"ipconfig /flushdns",
	} {
		require.False(t, isSafeReadOnlyCommand(command), command)
	}
}
