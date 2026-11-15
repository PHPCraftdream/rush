package shell

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: treating redirects as harmless (dropping the Redirects check)
// turns every `false` case with a redirection (`echo x > f`) `true`.
// Likewise treating `cd` as an ordinary wait command (adding it to
// waitCommands) turns `cd x && go test` `true`, while accepting `cd` anywhere
// instead of only left of `&&` turns `cd x` and `cd x; sleep 1` `true`.
func TestIsNoOpCommand(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		"sleep 90",
		"echo tick2",
		"sleep 5 && echo done",
		"true",
		":",
		"sleep 5; echo done; printf x",
		"cd D:/x && sleep 5",
		"cd x && cd y && sleep 1",
		"cd /tmp/w && sleep 600; echo w1",
		"cd \"C:/dir with space\" && sleep 2",
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
		"cd x && go test",
		"cd $(foo) && sleep 1",
		"cd $D && sleep 1",
		"cd x; sleep 1",
		"cd x",
		"cd a b && sleep 1",
		"cd a b c && sleep 1",
		"cd && sleep 1",
		"cd /tmp && rm -rf x",
		"cd /tmp && echo hi > f",
		"cd - && sleep 1",
		"cd -- && sleep 1",
		"cd /tmp && sleep 1 | cat",
		"cd /tmp && true || cat f",
	} {
		require.False(t, IsNoOpCommand(cmd), cmd)
	}
}
