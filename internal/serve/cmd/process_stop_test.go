package cmd

import (
	"context"
	"os"
	"syscall"
	"testing"

	tea "charm.land/bubbletea/v2"

	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
)

// Tests of newProcessStop/processStopAction live in cli/cmdkit/proc
// (process_stop_test.go, package cliproc) and of processStopKey in
// cli/cmdkit/out (cliui_test.go, package cliout) since lane K1 moved the
// implementation; the remaining cmd-side wiring tests stay here.

func TestProcessStopOwnsCommandSignals(t *testing.T) {
	restore := proc.SetProcessStopInstalledForTests(true)
	t.Cleanup(restore)
	if got := proc.CommandSignals(os.Interrupt, syscall.SIGTERM); len(got) != 0 {
		t.Fatalf("competing command signals: %v", got)
	}
	parent, cancel := context.WithCancel(t.Context())
	ctx, stop := proc.CommandSignalContext(parent, os.Interrupt)
	defer stop()
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("caller cancellation must still propagate")
	}
}

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
