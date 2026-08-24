package cmd

// The `sessions tail` subcommand: print the messages of a session and
// optionally follow it until the session finishes.

import (
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/spf13/cobra"
)

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Stream messages from a session",
	Long: `Output messages from a session, one block per message. By default,
prints all messages and exits. With --follow, polls for new messages
until the session finishes (last message has a non-Partial finish reason)
or until you press Ctrl+C.

Use --from-message <id> to resume from a specific message (skips earlier
messages). Use --format ndjson to emit JSON per line for piping into jq
or other tools.

Exit codes:
  0 — session completed or user interrupted with Ctrl+C
  1 — session not found
  2 — database error while streaming
  `,
	Args: cobra.ExactArgs(1),
	Example: `
# Print all messages and exit
rush sessions tail myid-123

# Live-tail a running session (Ctrl+C to stop)
rush sessions tail myid-123 --follow

# Resume from message abc123 in NDJSON format
rush sessions tail myid-123 --from-message abc123 --format ndjson

# Pipe to jq for filtering
rush sessions tail myid-123 --format ndjson | jq '.role == "assistant"'
  `,
	RunE: sessionsTailCmdRun,
}

func sessionsTailCmdRun(cmd *cobra.Command, args []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	fromMsgID, _ := cmd.Flags().GetString("from-message")
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

	sessionID := args[0]
	// Verify session exists
	_, err = resolveSessionID(cmd.Context(), a.Sessions, sessionID)
	if err != nil {
		return err
	}

	// Track the last message ID we've printed
	lastPrinted := fromMsgID

	// Print existing messages
	messages, err := a.Messages.List(cmd.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	// Filter by fromMsgID if set
	if fromMsgID != "" {
		found := false
		for i, msg := range messages {
			if msg.ID == fromMsgID {
				messages = messages[i+1:]
				found = true
				break
			}
		}
		if found {
			lastPrinted = fromMsgID
		}
	}

	// Build origin context from the snapshot we have right now; the
	// follow loop below extends it as new ToolCall parts arrive.
	callCtx := buildToolCallContext(messages)

	// Print messages. This batch ends at the session tail, so a row is
	// "followed by a later message" iff it isn't the last in the slice —
	// which is how finishReasonLabel distinguishes an auto-retried error
	// from a terminal one.
	now := time.Now()
	for i, msg := range messages {
		printMessageWithTime(os.Stdout, msg, format, now, callCtx, i < len(messages)-1)
		lastPrinted = msg.ID
	}

	// Opt-in (--with-subagents): render each sub-agent delegation's transcript
	// as a demarcated block after the parent stream. Rendered once (for the
	// snapshot at this point) rather than re-emitted on every follow tick, so
	// --follow doesn't repeat the whole child transcript each second.
	if withSubagents {
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sessionID, format, now)
	}

	if !follow {
		return nil
	}

	// Check if session is already finished
	isFinished := func() bool {
		msgs, err := a.Messages.List(cmd.Context(), sessionID)
		if err != nil || len(msgs) == 0 {
			return false
		}
		lastMsg := msgs[len(msgs)-1]
		if f := lastMsg.FinishPart(); f != nil && !f.Partial {
			return true
		}
		return false
	}

	// Poll for new messages
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		messages, err := a.Messages.List(cmd.Context(), sessionID)
		if err != nil {
			return fmt.Errorf("database error: %w", err)
		}

		// Rebuild origin context — new ToolCall parts may have arrived
		// this tick, and the next ToolResult render needs them.
		callCtx = buildToolCallContext(messages)

		// Print any new messages. lastIdx, not a CreatedAt/ID comparison:
		// messages is already the DB's own deterministic total order
		// (ListMessagesBySession's `ORDER BY created_at ASC, rowid ASC` —
		// created_at alone has only one-second granularity, so several
		// messages from one fast turn routinely tie). Re-deriving "after"
		// from CreatedAt+ID (the old isAfter) discarded that order and
		// substituted a coinflip on message.ID, a random UUID, whenever two
		// messages landed in the same second (task #319). Trusting the
		// slice's own position is both simpler and correct by construction.
		now := time.Now()
		lastIdx := indexByID(messages, lastPrinted)
		for i := range messages {
			if i > lastIdx {
				printMessageWithTime(os.Stdout, messages[i], format, now, callCtx, i < len(messages)-1)
				lastPrinted = messages[i].ID
			}
		}

		// Check if finished
		if isFinished() {
			return nil
		}
	}

	return nil
}

// indexByID returns the index of the message with the given id in
// messages, or -1 if id is empty or not found (including the case where a
// previously-printed message no longer appears in a fresh List() result —
// matching the old findByID+isAfter behavior of treating a missing anchor
// as "print everything").
func indexByID(messages []message.Message, id string) int {
	if id == "" {
		return -1
	}
	for i := range messages {
		if messages[i].ID == id {
			return i
		}
	}
	return -1
}
