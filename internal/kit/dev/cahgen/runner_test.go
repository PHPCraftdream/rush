package cahgen

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: npm resolves its Windows batch shim; node and Unix stay direct.
func TestRunnerCommand(t *testing.T) {
	for _, tc := range []struct{ platform, name string }{{"windows", "npm"}, {"windows", "node"}, {"linux", "npm"}} {
		t.Run(tc.platform+tc.name, func(t *testing.T) {
			calls := 0
			lookup := func(name string) (string, error) {
				calls++
				require.Equal(t, "npm.cmd", name)
				return "C:/Program Files/nodejs/npm.cmd", nil
			}
			name, args, err := runnerCommand(tc.platform, tc.name, []string{"pack", "--ignore-scripts"}, lookup)
			require.NoError(t, err)
			if tc.platform == "windows" && tc.name == "npm" {
				require.Equal(t, 1, calls)
				require.Equal(t, "cmd.exe", name)
				require.Equal(t, []string{"/d", "/s", "/c", "C:/Program Files/nodejs/npm.cmd", "pack", "--ignore-scripts"}, args)
			} else {
				require.Zero(t, calls)
				require.Equal(t, tc.name, name)
				require.Equal(t, []string{"pack", "--ignore-scripts"}, args)
			}
		})
	}
	sentinel := errors.New("missing npm.cmd")
	_, _, err := runnerCommand("windows", "npm", nil, func(string) (string, error) { return "", sentinel })
	require.ErrorIs(t, err, sentinel)
}
