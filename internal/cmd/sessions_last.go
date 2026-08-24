package cmd

// The `sessions last` subcommand: print the most recent N messages of a
// session, with the sub-agent activity pulse note.

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var sessionsLastCmd = &cobra.Command{
	Use:   "last <id>",
	Short: "Show the last N messages of a session",
	Long: `Print the most recent messages from a session without following.
Useful for quickly checking what an agent produced.

Use --n to control how many messages to show (default 10).
Use --format ndjson for machine-readable output.`,
	Example: `
# Show last 10 messages
rush sessions last myid-123

# Show last 3 messages
rush sessions last myid-123 --n 3

# Machine-readable
rush sessions last myid-123 --format ndjson | jq '.role'
`,
	Args: cobra.ExactArgs(1),
	RunE: sessionsLastCmdRun,
}

func sessionsLastCmdRun(cmd *cobra.Command, args []string) error {
	n, _ := cmd.Flags().GetInt("n")
	format, _ := cmd.Flags().GetString("format")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")
	if format != "text" && format != "ndjson" {
		return fmt.Errorf("invalid format: %s (must be text or ndjson)", format)
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Fix (pre-existing): resolveSessionID returned the full session but
	// the next call passed args[0] (which may be a short hash). On a
	// short-hash invocation that meant Messages.List got the hash, no
	// match, empty output. Use the resolved ID.
	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	messages, err := a.Messages.List(cmd.Context(), sess.ID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if len(messages) > n {
		messages = messages[len(messages)-n:]
	}
	// Build the tool-call context from the FULL message list (not just
	// the trimmed window) so a ToolResult inside the window can still
	// look up its matching ToolCall that may have been emitted earlier.
	callCtx := buildToolCallContext(messages)
	now := time.Now()
	// The window always ends at the true tail of the session (we trim from
	// the front), so a row is "followed by a later message" iff it isn't the
	// last one in the window. That's the signal finishReasonLabel uses to
	// tell a transient, auto-retried error from a terminal one.
	for i, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx, i < len(messages)-1)
	}

	// Opt-in (--with-subagents): after the parent's own stream, render each
	// sub-agent delegation's full transcript as a demarcated, indented block.
	// Default-hidden — without the flag we print only the parent rows (plus
	// the one-line pulse note below), never a child's message content inline.
	if withSubagents {
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sess.ID, format, now)
	}

	// Sub-agent pulse: `last` shows only the TOP-LEVEL session's rows, so an
	// in-flight `agent` delegation (which writes to a child session) is
	// invisible here. Append a one-line note when the freshest activity in
	// the call tree is a sub-agent's, so `last` doesn't look frozen while a
	// delegation is actively running. Text format only — ndjson consumers
	// get the structured signal from `sessions locks --json` / `show --json`.
	if format == "text" {
		if note := subAgentActivityNote(cmd.Context(), a, sess.ID, sess.UpdatedAt, now); note != "" {
			fmt.Fprintf(os.Stdout, "[%s]\n\n", note)
		}
	}
	return nil
}
