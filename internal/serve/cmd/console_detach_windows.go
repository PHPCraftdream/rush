//go:build windows

package cmd

import (
	"os"

	"golang.org/x/term"
)

var procFreeConsole = kernel32DLL.NewProc("FreeConsole")

// maybeDetachConsole detaches `rush run` from its console when ALL THREE
// standard streams are redirected (not a terminal) — the orchestrator
// launch pattern (`rush run < prompt > out 2> err`, often backgrounded
// with `&` from a wrapper shell that exits instantly).
//
// Why: when the wrapper shell exits and its console goes away, Windows
// sends CTRL_CLOSE_EVENT to every process still attached to that console —
// and for CTRL_CLOSE_EVENT the process is ALWAYS terminated once the
// handler chain returns; handling the event (installConsoleCtrlFilter)
// only suppresses the confirmation UI, the kill itself cannot be prevented
// from inside a handler. Observed in the wild: SessionAgent.Run started,
// lock created, dead before the first heartbeat tick, zero stderr, zero
// log entries — the OS TerminateProcess()'d it mid-boot.
//
// A bare FreeConsole (no console at all) is enough here: the earlier
// concern — mvdan.cc/sh's DefaultExecHandler spawning a new visible
// console per bash-tool command when rush itself has none to share — is
// now fixed at its own source (internal/shell/exec_windows.go hardens
// every child it spawns via platform.HideConsoleWindow), so rush doesn't
// need a console of its own for children to inherit. Giving rush its own
// console anyway (AllocConsole) was tried first and technically worked,
// but risks a brief visible flash before the immediate ShowWindow(SW_HIDE)
// takes effect — window creation and hiding aren't atomic. Not having a
// console at all has no such race.
//
// That same non-atomicity is why the child-side fix cannot rely on
// SysProcAttr.HideWindow alone: HideWindow is only SW_HIDE via
// STARTUPINFO, so a child's console window is likewise created and THEN
// hidden. platform.HideConsoleWindow therefore also passes
// CREATE_NO_WINDOW, under which no window is created to begin with — see
// its doc comment for the full story.
//
// The redirect-target file handles are not console handles, so they
// survive FreeConsole untouched and output keeps flowing into the
// redirect files.
//
// When any stream IS a terminal (an operator typed `rush run ...`
// interactively), we stay attached: Ctrl+C must keep working, and dying
// with the terminal tab is the expected interactive behavior.
func maybeDetachConsole() {
	if term.IsTerminal(int(os.Stdin.Fd())) ||
		term.IsTerminal(int(os.Stdout.Fd())) ||
		term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	_, _, _ = procFreeConsole.Call()
}
