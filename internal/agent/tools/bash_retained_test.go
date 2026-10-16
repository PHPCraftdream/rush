package tools

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestBashRetainedLaunchSeam(t *testing.T) {
	manager := shell.NewBackgroundShellManager()
	execution, err := startRetainedBashShell(t.Context(), manager, "owner", t.TempDir(), BashParams{Command: "echo retained-seam"})
	require.NoError(t, err)
	select {
	case <-execution.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("retained Bash launch did not complete")
	}
	require.NoError(t, manager.RemoveOwned("owner", execution.ID()))
	stdout, _, done, err := execution.GetOutput()
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, stdout, "retained-seam")
	require.NoError(t, execution.Dispose(t.Context()))
}
