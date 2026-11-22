package apiruns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/client/apiclient"
	apisessions "github.com/PHPCraftdream/rush/internal/serve/cmd/admin/serverctl/api/sessions"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	cliproc "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/proc"
	"github.com/spf13/cobra"
)

func NewAPIRunCmd() *cobra.Command {
	var role, model string
	var noWait, stream bool
	cmd := &cobra.Command{Use: `run SESSION "prompt"`, Short: "Run a prompt in a remote session", Long: "Create a run, waiting for its final answer by default. --stream prints events as they arrive; --no-wait prints the run id only." + apisessions.ApiLeafHelp, Example: `rush api run abc123 "Review this change" --role coder`, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if noWait && stream {
			return apisessions.ApiConfigurationError(errors.New("--no-wait cannot be combined with --stream"))
		}
		ctx, cancel, err := apisessions.ApiCommandContext(cmd, 0)
		if err != nil {
			return err
		}
		defer cancel()
		client, err := apisessions.NewAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, stop, interrupted := ApiSignalContext(ctx)
		defer stop()
		accepted, err := client.CreateRun(ctx, args[0], apiclient.RunRequest{Prompt: args[1], Role: role, Model: model}, "")
		if err != nil {
			return apisessions.ApiClientError(err)
		}
		if noWait {
			if apisessions.ApiJSON(cmd) {
				return apisessions.PrintAPIJSON(cmd, accepted)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), accepted.RunID)
			return err
		}
		if stream {
			return streamAPIRun(cmd, ctx, client, args[0], accepted.RunID, interrupted)
		}
		run, err := waitAPIRun(ctx, client, accepted.RunID)
		if err != nil {
			if interrupted() {
				apiCancelRemoteRun(cmd, client, accepted.RunID)
				return apisessions.InterruptedAPIError()
			}
			var paused *apiclient.RunPausedError
			if errors.As(err, &paused) {
				if paused.Run != nil {
					apiPrintRunPermissionHint(cmd, client, ctx, paused.Run)
				}
				return cliout.NewExitCodeError(6, err)
			}
			return apisessions.ApiClientError(err)
		}
		text, err := apiRunFinalText(ctx, client, run)
		if err != nil {
			return apisessions.ApiClientError(err)
		}
		if apisessions.ApiJSON(cmd) {
			return apisessions.PrintAPISingle(cmd, run)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), text)
		return err
	}}
	cmd.Flags().StringVar(&role, "role", "", "Role for the run")
	cmd.Flags().StringVar(&model, "model", "", "Model override for the run")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Print the run id without waiting")
	cmd.Flags().BoolVar(&stream, "stream", false, "Print events as they arrive")
	return cmd
}

// apiRunStatePollInterval bounds how long a finished run can go unnoticed when
// the event stream carries no usable run event.
var apiRunStatePollInterval = 3 * time.Second

// streamAPIRun prints events as they arrive and ends on the run's terminal
// (or paused) state with the same exit mapping as the non-stream path.
func streamAPIRun(cmd *cobra.Command, ctx context.Context, client *apiclient.Client, sessionID, runID string, interrupted func() bool) error {
	events, done := client.Events(ctx, sessionID, apiclient.EventsOptions{MaxReconnects: 1 << 30})
	poll := time.NewTicker(apiRunStatePollInterval)
	defer poll.Stop()
	// endByContext maps a cancelled command context to its exit: Ctrl+C
	// cancels the remote run and exits 130, a deadline exits 5.
	endByContext := func() error {
		if interrupted() {
			apiCancelRemoteRun(cmd, client, runID)
			return apisessions.InterruptedAPIError()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return cliout.NewExitCodeError(5, ctx.Err())
		}
		return apisessions.ApiClientError(ctx.Err())
	}
	check := func() (bool, error) {
		run, err := client.GetRun(ctx, runID, 0)
		if err != nil {
			if ctx.Err() != nil {
				return true, endByContext()
			}
			return true, apisessions.ApiClientError(err)
		}
		if !apiTerminalRun(run.State) && run.State != "awaiting_answer" {
			return false, nil
		}
		if run.State == "awaiting_answer" {
			apiPrintRunPermissionHint(cmd, client, ctx, run)
		}
		return true, apiRunStateExit(run)
	}
	for {
		select {
		case <-ctx.Done():
			return endByContext()
		case <-poll.C:
			if finished, err := check(); finished {
				return err
			}
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return endByContext()
				}
				if err := done(); err != nil {
					return apisessions.ApiClientError(err)
				}
				if finished, err := check(); finished {
					return err
				}
				return apisessions.ApiClientError(fmt.Errorf("event stream ended before run %s reached a terminal state", runID))
			}
			if apisessions.ApiJSON(cmd) {
				if err := apisessions.PrintAPIJSON(cmd, event); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", event.Data)
			}
			if event.Type == "run" {
				if finished, err := check(); finished {
					return err
				}
			}
		}
	}
}

const apiRunPollWaitSeconds = 25

func waitAPIRun(ctx context.Context, client *apiclient.Client, id string) (*apiclient.Run, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := client.GetRun(ctx, id, apiRunPollWaitSeconds)
		if err != nil {
			return nil, err
		}
		switch run.State {
		case "succeeded":
			return run, nil
		case "failed":
			if run.Error != nil {
				return nil, run.Error
			}
			return nil, errors.New("run failed")
		case "cancelled":
			return nil, errors.New("run cancelled")
		case "awaiting_answer":
			return run, &apiclient.RunPausedError{Run: run}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func apiTerminalRun(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

func apiRunStateExit(run *apiclient.Run) error {
	switch run.State {
	case "succeeded":
		return nil
	case "awaiting_answer":
		return cliout.NewExitCodeError(6, fmt.Errorf("run paused awaiting a human decision: %s", run.ID))
	case "failed", "cancelled":
		return cliout.NewExitCodeError(1, fmt.Errorf("run %s", run.State))
	default:
		return fmt.Errorf("run is not terminal: %s", run.State)
	}
}

func apiPrintRunPermissionHint(cmd *cobra.Command, client *apiclient.Client, ctx context.Context, run *apiclient.Run) {
	permissions, err := client.ListPermissions(ctx, apiclient.PermissionFilter{RunID: run.ID, State: "pending"})
	if err == nil && len(permissions) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "permission pending: %s\n", permissions[0].ID)
		fmt.Fprintf(cmd.ErrOrStderr(), "resolve with: rush api permissions allow %s\n", permissions[0].ID)
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "run paused awaiting a human decision: %s\n", run.ID)
}

func apiRunFinalText(ctx context.Context, client *apiclient.Client, run *apiclient.Run) (string, error) {
	if run.MessageID == "" {
		return "", nil
	}
	messages, err := client.AllMessages(ctx, run.SessionID, apisessions.ApiPageCap)
	if err != nil {
		return "", err
	}
	for _, message := range messages {
		if message.ID == run.MessageID {
			var parts []string
			for _, part := range message.Parts {
				if part.Type == "text" {
					parts = append(parts, part.Text)
				}
			}
			return strings.Join(parts, ""), nil
		}
	}
	return "", errors.New("final run message missing")
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

// apiNotifyContext is the signal seam: tests swap it because os.Interrupt cannot
// be delivered to a Windows test process.
var apiNotifyContext = cliproc.CommandSignalContext

// ApiSignalContext derives a context that SIGINT (Ctrl+C) cancels. interrupted
// reports synchronously whether the cancellation came from the signal rather
// than from the parent (deadline or caller); call it before stop.
func ApiSignalContext(parent context.Context) (ctx context.Context, stop context.CancelFunc, interrupted func() bool) {
	ctx, stop = apiNotifyContext(parent, os.Interrupt)
	return ctx, stop, func() bool { return parent.Err() == nil && ctx.Err() != nil }
}
