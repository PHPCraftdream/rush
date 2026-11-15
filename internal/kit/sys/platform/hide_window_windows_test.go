//go:build windows

package platform

import (
	"context"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestHideConsoleWindow_SetsCreateNoWindow is the regression test for the
// root cause of the long-running "a console window sometimes flashes on
// Windows" report.
//
// The original fix (d630d3a3) set only SysProcAttr.HideWindow and its
// comments asserted that this "sets the Windows CREATE_NO_WINDOW creation
// flag". It does not: Go's syscall/exec_windows.go maps HideWindow onto
// STARTUPINFO only (si.Flags |= STARTF_USESHOWWINDOW; si.ShowWindow =
// SW_HIDE) and composes the creation flags separately as
// `sys.CreationFlags | CREATE_UNICODE_ENVIRONMENT |
// _EXTENDED_STARTUPINFO_PRESENT`. SW_HIDE means the console window is
// created and *then* hidden — a race that loses under load, which is
// exactly why the flashing was reduced but never eliminated.
//
// Asserting on CreationFlags (not just HideWindow) is the whole point:
// a well-meaning revert to "HideWindow is enough" must fail here.
func TestHideConsoleWindow_SetsCreateNoWindow(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/c", "exit")
	HideConsoleWindow(cmd)

	require.NotNil(t, cmd.SysProcAttr, "SysProcAttr must be allocated")
	require.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW,
		"CREATE_NO_WINDOW must be set — HideWindow alone only hides an already-created window and races with it painting")
	require.True(t, cmd.SysProcAttr.HideWindow,
		"HideWindow must still be set — it covers GUI-subsystem children, for which CREATE_NO_WINDOW is ignored")
}

// TestHideConsoleWindow_PreservesExistingCreationFlags guards the OR-assign:
// a caller that already set a creation flag (or a future one added here)
// must not have it clobbered.
func TestHideConsoleWindow_PreservesExistingCreationFlags(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/c", "exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	HideConsoleWindow(cmd)

	require.NotZero(t, cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP,
		"a pre-existing creation flag must survive")
	require.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW,
		"CREATE_NO_WINDOW must be added alongside it")
}

// TestCommand_IsHardened pins that the sanctioned constructor actually
// applies the hardening — the guard test (TestNoUnhardenedProcessSpawns)
// only proves call sites go THROUGH Command, not that Command does
// anything.
func TestCommand_IsHardened(t *testing.T) {
	t.Parallel()

	cmd := Command(context.Background(), "cmd.exe", "/c", "exit")

	require.NotNil(t, cmd.SysProcAttr)
	require.NotZero(t, cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW)
	require.True(t, cmd.SysProcAttr.HideWindow)
}

// TestHideConsoleWindow_SpawnStillWorks is the "did the flag break normal
// execution" check: a CREATE_NO_WINDOW child must still run and still
// deliver its stdout through the pipe. This is the property that made
// CREATE_NO_WINDOW unsafe on the ConPTY path (measured there: 247 bytes
// of output without it, 0 with it) — that path deliberately does not use
// this function, and this test pins that ordinary pipe-backed spawns are
// unaffected.
func TestHideConsoleWindow_SpawnStillWorks(t *testing.T) {
	t.Parallel()

	cmd := Command(context.Background(), "cmd.exe", "/c", "echo", "hidden-ok")
	out, err := cmd.Output()
	require.NoError(t, err, "a CREATE_NO_WINDOW child must still execute normally")
	require.Contains(t, string(out), "hidden-ok",
		"stdout must still reach the parent through the pipe")
}
