package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

const apiEventsDefaultIdle = time.Second

func newAPIEventsCmd() *cobra.Command {
	var after string
	var follow bool
	var idle time.Duration
	cmd := &cobra.Command{Use: "events SESSION", Short: "Stream remote session events", Long: "Read session events using resumable SSE. Without --follow, exit after the replay has been idle for --idle; --follow keeps streaming until interrupted." + apiLeafHelp, Example: "rush api events abc123 --after 42 --follow", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var last uint64
		if after != "" {
			parsed, err := strconv.ParseUint(after, 10, 64)
			if err != nil {
				return apiConfigurationError(err)
			}
			last = parsed
		}
		if idle <= 0 {
			return apiConfigurationError(fmt.Errorf("--idle must be positive"))
		}
		ctx, cancel, err := apiCommandContext(cmd, 0)
		if err != nil {
			return err
		}
		defer cancel()
		ctx, stop, interrupted := apiSignalContext(ctx)
		defer stop()
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		// The stream has its own cancel: apiCommandContext's cancel is a no-op
		// when no --timeout is set, so it cannot stop the reader goroutine.
		streamCtx, stopStream := context.WithCancel(ctx)
		defer stopStream()
		events, done := client.Events(streamCtx, args[0], apiclient.EventsOptions{LastEventID: last, MaxReconnects: 1000})
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					if err := done(); err != nil {
						return apiClientError(err)
					}
					return nil
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
				if apiJSON(cmd) {
					if err := printAPIJSON(cmd, event); err != nil {
						return err
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n", event.Data)
				}
			case <-timer.C:
				if !follow {
					stopStream()
					if err := done(); err != nil && !errors.Is(err, context.Canceled) {
						return apiClientError(err)
					}
					return nil
				}
				timer.Reset(idle)
			case <-ctx.Done():
				if interrupted() {
					return interruptedAPIError()
				}
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return &exitCodeError{code: 5, err: ctx.Err()}
				}
				return apiClientError(ctx.Err())
			}
		}
	}}
	cmd.Flags().StringVar(&after, "after", "", "Resume after this numeric event id")
	cmd.Flags().BoolVar(&follow, "follow", false, "Follow the event stream until interrupted")
	cmd.Flags().DurationVar(&idle, "idle", apiEventsDefaultIdle, "Idle duration before non-follow replay exits")
	return cmd
}
