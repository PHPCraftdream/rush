package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

const apiBatchWaitNote = " A node held on a question stops the wait with exit code 6: answer it with rush api batch answer and wait again."

func newAPITaskCmd() *cobra.Command {
	var resume string
	var taskTimeout time.Duration
	var noWait bool
	cmd := &cobra.Command{Use: `task "prompt"`, Short: "Run one prompt as a remote batch task", Long: "Submit one prompt as a one-item batch through POST /tasks and wait for its output by default; --no-wait prints the batch root id only. --resume continues an earlier child session of your own batches. This is the work route of batch-bound tokens." + apiBatchWaitNote + apiLeafHelp, Example: `rush api task "Summarise the open TODOs"
rush api task --resume 4f2a9c "and now fix the first one" --no-wait`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if taskTimeout < 0 {
			return apiConfigurationError(errors.New("--task-timeout must not be negative"))
		}
		return apiBatchSubmitAndReport(cmd, noWait, func(ctx context.Context, client *apiclient.Client) (apiclient.BatchAccepted, error) {
			return client.CreateTask(ctx, apiclient.TaskRequest{Prompt: args[0], ResumeSessionID: resume, TimeoutSeconds: int(taskTimeout / time.Second)}, "")
		})
	}}
	cmd.Flags().StringVar(&resume, "resume", "", "Continue this child session of your own batches")
	cmd.Flags().DurationVar(&taskTimeout, "task-timeout", 0, "Per-task time limit (0 = server default, capped by the server)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Print the batch root id without waiting")
	return cmd
}

func newAPIBatchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "batch", Short: "Submit and follow remote batches", Long: "Submit a batch in the external batch schema of rush batch run, follow it, read node output, stop it and answer held questions." + apiLeafHelp, Example: "rush api batch run spec.json --no-wait", Args: cobra.NoArgs}
	cmd.AddCommand(newAPIBatchRunCmd(), newAPIBatchShowCmd(), newAPIBatchWaitCmd(), newAPIBatchStopCmd(), newAPIBatchOutputCmd(), newAPIBatchAnswerCmd())
	return cmd
}

func newAPIBatchRunCmd() *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{Use: "run [FILE]", Short: "Submit a batch spec", Long: "Read a batch spec (JSON, the external schema of rush batch run) from FILE, or from standard input when FILE is omitted or -, submit it through POST /batches and wait for it by default; --no-wait prints the batch root id only." + apiBatchWaitNote + apiLeafHelp, Example: "rush api batch run spec.json\ncat spec.json | rush api batch run --no-wait", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := readAPIBatchSpec(cmd, args)
		if err != nil {
			return err
		}
		return apiBatchSubmitAndReport(cmd, noWait, func(ctx context.Context, client *apiclient.Client) (apiclient.BatchAccepted, error) {
			return client.CreateBatch(ctx, spec, "")
		})
	}}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Print the batch root id without waiting")
	return cmd
}

func readAPIBatchSpec(cmd *cobra.Command, args []string) ([]byte, error) {
	var reader io.Reader = cmd.InOrStdin()
	if len(args) == 1 && args[0] != "-" {
		file, err := os.Open(args[0])
		if err != nil {
			return nil, apiConfigurationError(fmt.Errorf("open batch spec: %w", err))
		}
		defer file.Close()
		reader = file
	}
	spec, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err != nil {
		return nil, apiConfigurationError(fmt.Errorf("read batch spec: %w", err))
	}
	if len(spec) > 1<<20 {
		return nil, apiConfigurationError(errors.New("batch spec is larger than 1 MiB"))
	}
	if strings.TrimSpace(string(spec)) == "" {
		return nil, apiConfigurationError(errors.New("batch spec is empty"))
	}
	return spec, nil
}

// apiBatchSubmitAndReport submits, then (unless noWait) waits and prints the
// task output. Ctrl+C stops the batch it submitted and exits 130.
func apiBatchSubmitAndReport(cmd *cobra.Command, noWait bool, submit func(context.Context, *apiclient.Client) (apiclient.BatchAccepted, error)) error {
	ctx, cancel, err := apiCommandContext(cmd, 0)
	if err != nil {
		return err
	}
	defer cancel()
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, stop, interrupted := apiSignalContext(ctx)
	defer stop()
	accepted, err := submit(ctx, client)
	if err != nil {
		return apiClientError(err)
	}
	if noWait {
		if apiJSON(cmd) {
			return printAPIJSON(cmd, accepted)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), accepted.RootID)
		return err
	}
	return apiBatchWaitAndReport(cmd, ctx, client, accepted.RootID, interrupted, true)
}

// stopOnInterrupt is true for a batch this invocation submitted; a plain wait
// leaves the remote batch running.
func apiBatchWaitAndReport(cmd *cobra.Command, ctx context.Context, client *apiclient.Client, root string, interrupted func() bool, stopOnInterrupt bool) error {
	batch, err := client.WaitBatch(ctx, root)
	if err != nil {
		if interrupted() {
			if stopOnInterrupt {
				apiStopRemoteBatch(cmd, client, root)
			}
			return interruptedAPIError()
		}
		var paused *apiclient.BatchPausedError
		if errors.As(err, &paused) {
			apiPrintBatchQuestionHint(cmd, root, paused.Batch)
			return &exitCodeError{code: 6, err: err}
		}
		var failed *apiclient.BatchFailedError
		if errors.As(err, &failed) {
			if apiJSON(cmd) {
				_ = printAPIJSON(cmd, failed.Batch)
			} else {
				apiPrintBatchFailure(cmd, failed.Batch)
			}
			return &exitCodeError{code: 1, err: err}
		}
		return apiClientError(err)
	}
	if apiJSON(cmd) {
		return printAPIJSON(cmd, batch)
	}
	return apiPrintBatchOutputs(cmd, batch, "")
}

// apiStopRemoteBatch asks the server to stop a root this invocation submitted,
// on a fresh short context because the command context is already cancelled.
func apiStopRemoteBatch(cmd *cobra.Command, client *apiclient.Client, root string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.StopBatch(ctx, root, ""); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "remote stop unconfirmed for batch %s: %v\n", root, err)
	}
}

func apiPrintBatchQuestionHint(cmd *cobra.Command, root string, batch *apiclient.Batch) {
	for _, node := range batch.HeldQuestions() {
		fmt.Fprintf(cmd.ErrOrStderr(), "question pending on node %s: %s\n", node.ID, node.Question)
		fmt.Fprintf(cmd.ErrOrStderr(), "answer with: rush api batch answer %s %s \"<text>\"\n", root, node.ID)
	}
}

func apiPrintBatchFailure(cmd *cobra.Command, batch *apiclient.Batch) {
	fmt.Fprintf(cmd.ErrOrStderr(), "batch %s %s\n", batch.RootID, batch.State)
	for _, node := range batch.Nodes {
		if node.Kind == "task" && node.State != "completed" {
			fmt.Fprintf(cmd.ErrOrStderr(), "  node %s %s %s %s\n", node.ID, node.State, node.OutcomeCode, node.OutcomeSummary)
		}
	}
}

// apiBatchTaskNodes returns the task nodes in execution order, optionally one node.
func apiBatchTaskNodes(batch *apiclient.Batch, only string) []apiclient.BatchNode {
	var tasks []apiclient.BatchNode
	for _, node := range batch.Nodes {
		if node.Kind == "task" && (only == "" || node.ID == only) {
			tasks = append(tasks, node)
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].Ordinal < tasks[j].Ordinal })
	return tasks
}

// apiPrintBatchOutputs prints each task node's output; a single node prints bare.
func apiPrintBatchOutputs(cmd *cobra.Command, batch *apiclient.Batch, only string) error {
	tasks := apiBatchTaskNodes(batch, only)
	if only != "" && len(tasks) == 0 {
		return &exitCodeError{code: 4, err: fmt.Errorf("node %s not found in batch %s", only, batch.RootID)}
	}
	for _, node := range tasks {
		if node.OutputExpired && node.Output == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "output of node %s has expired (outcome: %s %s)\n", node.ID, node.OutcomeCode, node.OutcomeSummary)
			continue
		}
		if len(tasks) > 1 {
			fmt.Fprintf(cmd.OutOrStdout(), "== %s ==\n", node.ID)
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(node.Output, "\n")); err != nil {
			return err
		}
	}
	return nil
}

func newAPIBatchShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show ROOT", Short: "Show a batch and its nodes", Long: "Print the batch state and its nodes: kind, state, tool, child session and whether a node waits on a question. --json prints the batch object." + apiLeafHelp, Example: "rush api batch show 8d409841", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			batch, err := c.GetBatch(ctx, args[0])
			if err != nil {
				return apiClientError(err)
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, batch)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "batch %s %s\n", batch.RootID, batch.State)
			return printAPIItems(cmd, apiBatchNodeRows(batch), []string{"id", "kind", "state", "tool", "child_session_id", "awaiting_answer"})
		})
	}}
}

func apiBatchNodeRows(batch *apiclient.Batch) []apiclient.BatchNode {
	rows := append([]apiclient.BatchNode(nil), batch.Nodes...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Ordinal < rows[j].Ordinal })
	return rows
}

func newAPIBatchWaitCmd() *cobra.Command {
	return &cobra.Command{Use: "wait ROOT", Short: "Wait for a batch to finish", Long: "Poll a batch until it is terminal and print the task output; exit code 1 when it did not complete. Ctrl+C stops waiting and the remote batch is left running." + apiBatchWaitNote + apiLeafHelp, Example: "rush api batch wait 8d409841", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel, err := apiCommandContext(cmd, 0)
		if err != nil {
			return err
		}
		defer cancel()
		client, err := newAPIClient(cmd)
		if err != nil {
			return err
		}
		ctx, stop, interrupted := apiSignalContext(ctx)
		defer stop()
		return apiBatchWaitAndReport(cmd, ctx, client, args[0], interrupted, false)
	}}
}

func newAPIBatchStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop ROOT [NODE]", Short: "Stop a batch or one node", Long: "Ask the server to stop the whole batch, or one node of it. The stop is queued; poll with rush api batch show." + apiLeafHelp, Example: "rush api batch stop 8d409841\nrush api batch stop 8d409841 8d409841.1", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			node := ""
			if len(args) == 2 {
				node = args[1]
			}
			accepted, err := c.StopBatch(ctx, args[0], node)
			if err != nil {
				return apiClientError(err)
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, accepted)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "stop %s\n", accepted.State)
			return err
		})
	}}
}

func newAPIBatchOutputCmd() *cobra.Command {
	return &cobra.Command{Use: "output ROOT [NODE]", Short: "Print the output of a batch's task nodes", Long: "Print the output of every task node of a batch, or of one node. Live output is kept for ten minutes after the batch finishes; an expired output is reported on standard error and the command exits 0." + apiLeafHelp, Example: "rush api batch output 8d409841", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			batch, err := c.GetBatch(ctx, args[0])
			if err != nil {
				return apiClientError(err)
			}
			node := ""
			if len(args) == 2 {
				node = args[1]
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, apiBatchTaskNodes(batch, node))
			}
			return apiPrintBatchOutputs(cmd, batch, node)
		})
	}}
}

func newAPIBatchAnswerCmd() *cobra.Command {
	return &cobra.Command{Use: `answer ROOT NODE "text"`, Short: "Answer a question a node is held on", Long: "Deliver an answer to the node's existing execution; it continues the same run. Exit code 4 when the batch or node is unknown, 1 when the node has no current question." + apiLeafHelp, Example: `rush api batch answer 8d409841 8d409841.1 "yes, use the staging database"`, Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		return apiWithClient(cmd, func(c *apiclient.Client, ctx context.Context) error {
			if err := c.AnswerBatchQuestion(ctx, args[0], args[1], args[2]); err != nil {
				return apiClientError(err)
			}
			if apiJSON(cmd) {
				return printAPIJSON(cmd, map[string]any{"node_id": args[1], "delivered": true})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "answered %s\n", args[1])
			return err
		})
	}}
}
