package cmd

import (
	"context"
	"os"
	"syscall"
	"testing"

	proc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
)

// Tests of newProcessStop/processStopAction live in cli/cmdkit/proc
// (process_stop_test.go, package cliproc) and of processStopKey in
// cli/cmdkit/out (cliui_test.go, package cliout) since lane K1 moved the
// implementation; the remaining cmd-side wiring tests stay here. The
// oauthLinkModel Ctrl-C wiring test moved to admin/login (lane M5).

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
