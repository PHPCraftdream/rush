//go:build windows

package platform

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideConsoleWindow suppresses the console window of a spawned
// console-subsystem child (rg, npm.cmd, node, git, taskkill, ...).
// Needed whenever rush itself may have no console of its own to share —
// e.g. a detached/orchestrator run, where cmd.maybeDetachConsole calls
// FreeConsole() as soon as stdin/stdout/stderr are not terminals — in
// which case Windows allocates a fresh console per spawn. Safe to call
// unconditionally: it's a no-op UX improvement even when rush does have
// a console (redirected output keeps flowing to Stdout/Stderr either
// way, only the window is suppressed).
//
// Both mechanisms below are set, deliberately, because they act at
// different moments and neither alone is sufficient:
//
//   - CREATE_NO_WINDOW is a process-creation flag: the console is created
//     WITHOUT a window in the first place. This is the load-bearing half.
//   - HideWindow (STARTF_USESHOWWINDOW | SW_HIDE) only asks for the
//     window to be shown hidden, i.e. the window IS created and then
//     hidden. That is a race, and under load the window can paint before
//     the hide lands — the "sometimes a console flashes" report this
//     function exists to prevent. It is kept because it also covers
//     GUI-subsystem children, for which CREATE_NO_WINDOW is ignored.
//
// This distinction was missed when the original fix (d630d3a3) landed:
// its comments claimed SysProcAttr.HideWindow "sets the Windows
// CREATE_NO_WINDOW creation flag". It does not — verified against
// Go 1.26.3's syscall/exec_windows.go, where HideWindow only touches
// STARTUPINFO (si.Flags |= STARTF_USESHOWWINDOW; si.ShowWindow =
// SW_HIDE) while the creation flags are composed independently as
// `sys.CreationFlags | CREATE_UNICODE_ENVIRONMENT |
// _EXTENDED_STARTUPINFO_PRESENT`. So CREATE_NO_WINDOW was never actually
// being passed, which is why the flashing was reduced but never
// eliminated. The original commit's own repro — an `npx`-based stdio MCP
// server spawning npx.cmd -> cmd.exe -> node.exe — was still reproducing
// months later.
//
// NOT applicable to the ConPTY spawn in internal/agent/cliprovider: that
// path uses go-pty's own Cmd (not an *exec.Cmd) and never reaches this
// function. CREATE_NO_WINDOW was separately measured to break a pseudo
// console outright there (247 bytes of child output without it, 0 with
// it), so it must not be propagated to that path — see the note on
// [Command].
func HideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
