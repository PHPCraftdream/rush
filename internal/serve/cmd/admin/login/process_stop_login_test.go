package login

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
)

// TestProcessStopRawCtrlCLogin moved here from internal/serve/cmd/
// process_stop_test.go (lane M5): it white-boxes the oauthLinkModel that now
// lives in package login.
func TestProcessStopRawCtrlCLogin(t *testing.T) {
	var code int
	restore := proc.SwapActiveProcessStopForTests(func(c int) { code = c })
	t.Cleanup(restore)
	msg := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	model := &oauthLinkModel{interrupt: func() { t.Error("Ctrl+C fell through to graceful login cancellation") }}
	model.Update(msg)
	if code != 130 {
		t.Fatal("physical login Ctrl+C did not request process stop")
	}
}
