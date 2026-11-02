package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

// apiSignalContext derives a context that SIGINT (Ctrl+C) cancels. interrupted
// reports synchronously whether the cancellation came from the signal rather
// than from the parent (deadline or caller); call it before stop.
// apiNotifyContext is the signal seam: tests swap it because os.Interrupt cannot
// be delivered to a Windows test process.
var apiNotifyContext = signal.NotifyContext

func apiSignalContext(parent context.Context) (ctx context.Context, stop context.CancelFunc, interrupted func() bool) {
	ctx, stop = apiNotifyContext(parent, os.Interrupt)
	return ctx, stop, func() bool { return parent.Err() == nil && ctx.Err() != nil }
}

// apiCancelRemoteRun asks the server to cancel a run this invocation submitted,
// on a fresh short context because the command context is already cancelled.
func apiCancelRemoteRun(cmd *cobra.Command, client *apiclient.Client, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.CancelRun(ctx, runID); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "remote cancellation unconfirmed for run %s: %v\n", runID, err)
	}
}

func interruptedAPIError() error { return &exitCodeError{code: 130, err: fmt.Errorf("interrupted")} }
