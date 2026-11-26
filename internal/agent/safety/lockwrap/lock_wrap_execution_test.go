package lockwrap

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunLockedCommandExitCode(t *testing.T) {
	raw, argv := "sh -c \"exit 3\"", []string{"sh", "-c", "exit 3"}
	if runtime.GOOS == "windows" {
		raw, argv = "cmd /c exit 3", []string{"cmd", "/c", "exit", "3"}
	}
	stdout, stderr, code, err := RunLockedCommand(context.Background(), "lane117-exit-code-test", raw, t.TempDir(), argv)
	require.NoError(t, err)
	require.Equal(t, 3, code)
	require.Empty(t, stdout)
	require.Empty(t, stderr)
}

func TestLockWrapCommandEligibility(t *testing.T) {
	wrap, name := Decision([]string{"exact:cmd /c exit 0"}, "cmd /c exit 0", false, false)
	require.True(t, wrap)
	require.Equal(t, "cmd-c-exit-0", name)
	wrap, _ = Decision([]string{"cmd"}, "cmd /c exit 0", true, false)
	require.False(t, wrap)
	wrap, _ = Decision([]string{"cmd"}, "cmd /c exit 0", false, false)
	require.True(t, wrap)
}
