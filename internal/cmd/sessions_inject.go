package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsInjectCmd = &cobra.Command{
	Use:   "inject <session-id>",
	Short: "Inject a user message into a session from another process",
	Long: `Inject a message into a running (or at-rest) session as if the user
had typed it. The message is persisted immediately as a normal user
message — it renders in the web UI exactly like anything the user sends —
and a cross-process "pending inject" signal is queued so that whichever
process is currently running the session splices it into the live prompt.

The message text comes from either -m/--message (inline) or -f/--file
(read from a UTF-8 file). Exactly one of the two must be given.

If the session is currently running in another process, the message is
merged into its next provider request without restarting the turn. With
--interrupt the inject is marked so the running turn is cancelled and
restarted with the new message (interrupt handling itself lives in the
running process). A "rush run" loop waiting between turns (on a job, a
delegation or a retry) counts as running the session: the result says
running:true, between_turns:true and driver_pid (JSON), no turn is
running to interrupt, and the message reaches the loop's next turn if it
runs one (otherwise the session's next run). If no process is running the
session, the message is still persisted and will be picked up the next
time the session runs.

The <session-id> may be a full session id or a hash prefix as printed by
"sessions list".`,
	Args: cobra.ExactArgs(1),
	Example: `
# Inline message
rush sessions inject pr-42 -m "also update the changelog"

# From a file
rush sessions inject pr-42 -f ./notes/next-step.md

# Interrupt the current turn and restart with this message
rush sessions inject pr-42 -m "stop, wrong approach" --interrupt

# Match by hash prefix, machine-readable result
rush sessions inject 8a3f0c -m "continue" --json
  `,
	RunE: sessionsInjectCmdRun,
}

func init() {
	sessionsInjectCmd.Flags().StringP("message", "m", "", "Message text to inject (mutually exclusive with --file)")
	sessionsInjectCmd.Flags().StringP("file", "f", "", "Read message text from this file (mutually exclusive with --message)")
	sessionsInjectCmd.Flags().Bool("interrupt", false, "Cancel the current turn and restart it with this message")
	sessionsInjectCmd.Flags().Bool("json", false, "Emit a structured JSON result")

	sessionsCmd.AddCommand(sessionsInjectCmd)
}

// injectResult is the wire shape of `rush sessions inject --json`.
type injectResult struct {
	SessionID string `json:"session_id"`
	Hash      string `json:"hash"`
	MessageID string `json:"message_id"`
	Interrupt bool   `json:"interrupt"`
	Running   bool   `json:"running"`
	Status    string `json:"status"` // injected | queued-for-interrupt | persisted-offline
	// BetweenTurns: a live `rush run` loop drives the session but no turn is
	// running (it waits on a job, a delegation or a retry); DriverPID is the
	// loop's. The message reaches the loop's next turn, if it runs one.
	BetweenTurns bool `json:"between_turns,omitempty"`
	DriverPID    int  `json:"driver_pid,omitempty"`
}

func sessionsInjectCmdRun(cmd *cobra.Command, args []string) error {
	inline, _ := cmd.Flags().GetString("message")
	file, _ := cmd.Flags().GetString("file")
	interrupt, _ := cmd.Flags().GetBool("interrupt")
	asJSON, _ := cmd.Flags().GetBool("json")

	text, err := resolveInjectText(inline, file)
	if err != nil {
		return err
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, msg, err := doInject(cmd.Context(), a.Sessions, a.Messages, args[0], text, interrupt)
	if err != nil {
		return err
	}

	// A session is running while a process holds its lock (a turn) or a live
	// `rush run` loop drives it between turns (lock released, driver marker
	// live: R7C-3).
	running := isSessionLockAlive(a.Config().Options.DataDirectory, sess.ID)
	var loop *session.SessionDriver
	if !running {
		if loop = inspectSessionLiveWork(cmd.Context(), a, sess.ID).driver; loop != nil {
			running = true
		}
	}

	status := "injected"
	switch {
	case !running:
		status = "persisted-offline"
	case interrupt:
		status = "queued-for-interrupt"
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(injectResult{
			SessionID: sess.ID,
			Hash:      session.HashID(sess.ID),
			MessageID: msg.ID,
			Interrupt: interrupt,
			Running:   running,
			Status:    status,

			BetweenTurns: loop != nil,
			DriverPID:    loopPID(loop),
		})
	}

	if loop != nil {
		verb, note := "injected into", ""
		if interrupt {
			verb = "queued for interrupt on"
			note = "; --interrupt has no running turn to cancel and applies when that turn starts"
		}
		fmt.Fprintf(os.Stderr, "%s session %s (%s): rush run PID %d drives it between turns; no turn is running now, so it reaches the loop's next turn (if it runs one, else the session's next run)%s\n",
			verb, sess.ID, short(session.HashID(sess.ID)), loop.PID, note)
		return nil
	}

	switch status {
	case "persisted-offline":
		fmt.Fprintf(os.Stderr, "message persisted; no process is currently running this session — it will be picked up when the session next runs\n")
	case "queued-for-interrupt":
		fmt.Fprintf(os.Stderr, "queued for interrupt on session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	default:
		fmt.Fprintf(os.Stderr, "injected into session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	}
	return nil
}

func loopPID(d *session.SessionDriver) int {
	if d == nil {
		return 0
	}
	return int(d.PID)
}

// resolveInjectText validates the -m/-f pair (exactly one required) and
// returns the message text, reading from the file when -f is given.
func resolveInjectText(inline, file string) (string, error) {
	switch {
	case inline == "" && file == "":
		return "", fmt.Errorf("no message given: pass exactly one of -m/--message or -f/--file")
	case inline != "" && file != "":
		return "", fmt.Errorf("both -m/--message and -f/--file given: pass exactly one")
	}
	text := inline
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("failed to read message file %s: %w", file, err)
		}
		text = string(data)
	}
	if text == "" {
		return "", fmt.Errorf("message text is empty")
	}
	return text, nil
}

// doInject resolves the session, persists a normal user message (so the web
// UI renders it as user-typed), and queues the cross-process pending-inject
// signal referencing that message row. Split out from RunE so it is testable
// without a full app/config bootstrap.
func doInject(
	ctx context.Context,
	sessions session.Service,
	messages message.Service,
	idOrHash, text string,
	interrupt bool,
) (session.Session, message.Message, error) {
	sess, err := resolveSessionID(ctx, sessions, idOrHash)
	if err != nil {
		return session.Session{}, message.Message{}, err
	}

	// Mirrors sessionAgent.createUserMessage's CreateMessageParams shape so
	// the message renders identically to anything the user sends.
	msg, err := messages.Create(ctx, sess.ID, message.CreateMessageParams{
		// CLI inject is its own entry channel, even into an SDK/web-created session.
		Origin: message.OriginCLI,
		Role:   message.User,
		Parts:  []message.ContentPart{message.TextContent{Text: text}},
	})
	if err != nil {
		return session.Session{}, message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}

	if err := sessions.CreatePendingInject(ctx, session.PendingInject{
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   text,
		Interrupt: interrupt,
	}); err != nil {
		return session.Session{}, message.Message{}, fmt.Errorf("failed to queue pending inject: %w", err)
	}
	return sess, msg, nil
}

// isSessionLockAliveThreshold mirrors lockPulseStatus's (sessions.go) "offline"
// cutoff of 20s, and internal/server/handlers.go's externalOwnerLiveThreshold —
// so the mtime-fresh fast path below behaves identically to before this was
// rewritten to delegate to session.InspectSessionLock.
const isSessionLockAliveThreshold = 20 * time.Second

// isSessionLockAlive reports whether a live process currently holds this
// session's lock. It delegates to session.InspectSessionLock, which checks
// the heartbeat mtime as a fast path and, only when that mtime already looks
// stale, falls back to a real session.IsProcessAlive(pid) probe before
// concluding the holder is dead (task #228: the heartbeat's mtime touch is
// gated on real activity, and the stream watchdog tick that supplies that
// activity during a long tool call can lag past this threshold even for a
// perfectly healthy session).
//
// On Windows, the exclusive lock prevents reading the PID while the holder
// is alive, so a fresh lock with PID 0 is unaffected by the PID fallback —
// InspectSessionLock only attempts the PID probe once mtime is already
// stale, so a fresh lock with an unreadable PID still reports Live: true via
// the mtime fast path alone.
func isSessionLockAlive(dataDir, sessionID string) bool {
	if dataDir == "" || sessionID == "" {
		return false
	}
	return session.InspectSessionLock(dataDir, sessionID, isSessionLockAliveThreshold).Live
}
