package shell

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: treating redirects as harmless (dropping the Redirects check)
// turns every `false` case with a redirection (`echo x > f`) `true`.
func TestIsNoOpCommand(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		"sleep 90",
		"echo tick2",
		"sleep 5 && echo done",
		"true",
		":",
		"sleep 5; echo done; printf x",
	} {
		require.True(t, IsNoOpCommand(cmd), cmd)
	}
	for _, cmd := range []string{
		"sleep 90; gh run view",
		"echo x > f",
		"echo $(date)",
		"sleep $N",
		"cat f",
		"sleep 5 | cat",
		"true || cat f",
		"go test ./...",
		"",
	} {
		require.False(t, IsNoOpCommand(cmd), cmd)
	}
}
