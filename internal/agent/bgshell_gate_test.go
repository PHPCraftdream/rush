package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// bgshell_gate_test.go: portable natural-completion fixtures for background
// shells. A background shell that ends NATURALLY stays in the manager's table
// (only Kill/KillOwned/KillAll Remove it), so its completion produces the wake
// notice; a shell absent from the manager reads as cancelled-by-stop-control
// (bgShellRowCancelled) and its completion wakes NOTHING. Fixtures that need
// the wake therefore end the shell naturally: the shell blocks on a gate file
// (a bash polling loop — portable on Windows/Linux/macOS, unlike a FIFO) and
// the fixture releases it by creating the file.

// bgGateName is the gate file every hold command polls, relative to the
// shell's working directory.
const bgGateName = "bg_shell_gate"

// bgHoldCommand returns a POSIX-shell command that blocks until the gate file
// appears in the shell's working directory and then exits 0.
func bgHoldCommand(gate string) string {
	return "until [ -e " + gate + " ]; do sleep 0.05; done"
}

// releaseBGShellGate creates the gate file in dir, ending every shell that
// bgHoldCommand started there.
func releaseBGShellGate(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, bgGateName), []byte("go"), 0o644))
}
