package sessscan

// The `sessions tail` subcommand: print the messages of a session and
// optionally follow it until the session finishes.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	sessview "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view"
	"github.com/spf13/cobra"
)

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Stream messages from a session",
	Long: `Output messages from a session, one block per message. By default,
prints all messages and exits. With --follow, polls for new messages
until the session finishes (last message has a non-Partial finish reason and
no live "rush run" loop, running job or delegation still works on it: the
lock is held only during a turn, so a first turn that yields is not the end)
or until you press Ctrl+C.

Use --from-message <id> to resume from a specific message (skips earlier
messages). Use --format ndjson to emit JSON per line for piping into jq
or other tools.

Exit codes:
  0 — session completed or user interrupted with Ctrl+C
  1 — session not found, or a database error while streaming
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
	RunE: func(cmd *cobra.Command, args []string) error {
		return sessionsTailCmdRun(cmd, args, cmdkit.SetupApp)
	},
}

var sessionsTailFlagsOnce sync.Once

// NewSessionsTailCmd returns a fresh `sessions tail` command bound to the
// given app setup (DeepFreshCmd keeps per-invocation flag state isolated).
func NewSessionsTailCmd(setup func(*cobra.Command) (*app.App, error)) *cobra.Command {
	sessionsTailFlagsOnce.Do(func() {
		sessionsTailCmd.Flags().Bool("follow", false, "Keep polling for new messages until session finishes")
		sessionsTailCmd.Flags().String("from-message", "", "Resume from this message ID (skip earlier)")
		sessionsTailCmd.Flags().String("format", "text", "Output format: text or ndjson")
		sessionsTailCmd.Flags().Bool("with-subagents", false, "After the parent stream, render each sub-agent delegation's transcript as a demarcated block (snapshot; not re-emitted while --follow)")
	})
	cmd := cmdkit.DeepFreshCmd(sessionsTailCmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return sessionsTailCmdRun(cmd, args, setup)
	}
	return cmd
}

// tailFollowStartedHook, when set by a test, runs once the existing messages
// are printed and --follow starts polling.
var tailFollowStartedHook func()

func sessionsTailCmdRun(cmd *cobra.Command, args []string, setup func(*cobra.Command) (*app.App, error)) error {
	follow, _ := cmd.Flags().GetBool("follow")
	fromMsgID, _ := cmd.Flags().GetString("from-message")
	format, _ := cmd.Flags().GetString("format")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")

	if format != "text" && format != "ndjson" {
		return fmt.Errorf("invalid format: %s (must be text or ndjson)", format)
	}

	a, err := setup(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// The argument may be a HASH prefix: every read below uses the resolved id.
	sess, err := cmdkit.ResolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}
	sessionID := sess.ID

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
	callCtx := sessview.BuildToolCallContext(messages)

	// Print messages. This batch ends at the session tail, so a row is
	// "followed by a later message" iff it isn't the last in the slice —
	// which is how finishReasonLabel distinguishes an auto-retried error
	// from a terminal one.
	now := time.Now()
	for i, msg := range messages {
		sessview.PrintMessageWithTime(os.Stdout, msg, format, now, callCtx, i < len(messages)-1)
		lastPrinted = msg.ID
	}

	// Opt-in (--with-subagents): render each sub-agent delegation's transcript
	// as a demarcated block after the parent stream. Rendered once (for the
	// snapshot at this point) rather than re-emitted on every follow tick, so
	// --follow doesn't repeat the whole child transcript each second.
	if withSubagents {
		sessview.PrintSubAgentTranscripts(cmd.Context(), os.Stdout, a, sessionID, format, now)
	}

	if !follow {
		return nil
	}
	if tailFollowStartedHook != nil {
		tailFollowStartedHook()
	}

	workNoted := "" // last live-work note printed (one line per change)

	// Poll for new messages
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-cmd.Context().Done():
			return nil // Ctrl-C: exit 0, as documented
		case <-ticker.C:
		}
		messages, err := a.Messages.List(cmd.Context(), sessionID)
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil // interrupted mid-read
			}
			return fmt.Errorf("database error: %w", err)
		}

		// Rebuild origin context — new ToolCall parts may have arrived
		// this tick, and the next ToolResult render needs them.
		callCtx = sessview.BuildToolCallContext(messages)

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
		lastIdx := sessview.IndexByID(messages, lastPrinted)
		for i := range messages {
			if i > lastIdx {
				sessview.PrintMessageWithTime(os.Stdout, messages[i], format, now, callCtx, i < len(messages)-1)
				lastPrinted = messages[i].ID
			}
		}

		// Check if finished
		finished, work := tailSessionFinished(cmd.Context(), a, sessionID)
		if work != "" && work != workNoted {
			fmt.Fprintf(os.Stderr, "(between turns — %s; still following)\n", work)
			workNoted = work
		}
		if finished {
			return nil
		}
	}
}

// tailSessionFinished reports whether `sessions tail --follow` should stop:
// the newest message has a non-partial finish AND nothing keeps the session
// open. The lock is held only during a turn, so that finish can be the first
// turn's while a `rush run` loop waits on a job (R6C-1): a live driver marker,
// own job or delegation keeps the follow going, and the second result names
// it ("" when none).
func tailSessionFinished(ctx context.Context, a *app.App, sessionID string) (bool, string) {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return false, ""
	}
	lastMsg := msgs[len(msgs)-1]
	if f := lastMsg.FinishPart(); f == nil || f.Partial {
		return false, ""
	}
	act, actErr := a.SessionActivity(ctx, sessionID)
	if actErr != nil {
		return false, ""
	}
	if sesskit.KindIsLive(act.Verdict.Kind) {
		return false, act.Verdict.Description
	}
	return true, ""
}
