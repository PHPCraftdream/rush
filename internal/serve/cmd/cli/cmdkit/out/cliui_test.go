package cliout

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
)

// Moved from internal/serve/cmd/process_stop_test.go (lane K1): the
// processStopKey unit checks poke the unexported cliout helper; the
// oauthLinkModel half of the original test stays in package cmd.
func TestProcessStopRawCtrlC(t *testing.T) {
	var code int
	restore := proc.SwapActiveProcessStopForTests(func(c int) { code = c })
	t.Cleanup(restore)
	msg := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	if got := processStopKey(msg); got != nil || code != 130 {
		t.Fatalf("raw Ctrl+C did not stop: msg=%v code=%d", got, code)
	}
	code = 0
	plain := tea.KeyPressMsg{Code: 'c'}
	if got := processStopKey(plain); got == nil || code != 0 {
		t.Fatal("plain c must not stop the process")
	}
}
