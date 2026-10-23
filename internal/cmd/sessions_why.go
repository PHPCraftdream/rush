package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsWhyCmd = &cobra.Command{
	Use:   "why <id>",
	Short: "Explain why a session has the status it has",
	Long: `Print a one-shot diagnostic explaining a session's current status
(running / crashed / done / at rest) and the evidence behind it, using
only data rush itself owns: the session/message DB (including the async job
ledger) and the lock files -- session locks in .rush/locks, and the host locks
in .rush/hosts that are probed (read-only) to tell whether the process
running a job is alive.

This is the command to reach for when "sessions list" shows a session as\n"crashed" and you want to know whether it genuinely died mid-turn or
actually finished cleanly and left a stale lock behind. It does NOT read
external log files or orchestrator redirect output.

The five possible verdicts:

  done     — last assistant message finished with end_turn, no delegation
             is live and no background job of its own is running.
  crashed  — lock file exists, holder is dead (PID dead AND heartbeat
             stale), and no assistant message with a clean finish.
             Likely died mid-turn.
  running  — lock file exists, holder PID is alive OR the heartbeat is
             still fresh (PID alone is not trusted — on Windows it reads
             as unreadable for the entire lifetime of a live session). Also
             reported when the session has no live session lock (or a stale one with
             a clean finish) but still owns a RUNNING background job
             (bash/run_command) on a host that is not provably dead: the
             session is waiting on its own job, so it is NOT done. Also
             reported when a live rush run loop drives the session between
             turns (its durable driver marker names a host that is not
             provably dead, e.g. a paced retry after a failed reaction turn):
             the loop will still react, so it is NOT at rest.
  delegating — this session's own session lock is gone (or stale) but a
             delegation is still live: a held batch root names a
             DESCENDANT session (the process running it, which
             holds a per-process host lock) is alive or cannot be probed.
             Delegated sub-agent work is still in progress, so the session
             is NOT done even though its own turn yielded.
  at rest  — no session lock file. Not running, not crashed, no live
             delegation, no running background job of its own and no live
             rush run driver.

The verdict is the one session-activity classifier shared with\n"sessions list", "watch" and the web surfaces: live work (a live driver, a
running own job, a live delegation, an open once schedule) outranks a dead
recorded PID — the PID becomes an annotation, not the Kind. "crashed" stays
for a dead PID (or a dead-host marker/row) with NO live work: release wipes
the recorded PID, so a recorded PID means the holder never reached release,
even when the last finish was end_turn (that finish is a previous turn's).
The only end signal is the run's ended_reason.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Why does sessions list show this one as crashed?
rush sessions why pr-42

# Same, by hash prefix
rush sessions why 8a3f0c
  `,
	RunE: sessionsWhyCmdRun,
}

func sessionsWhyCmdRun(cmd *cobra.Command, args []string) error {
	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	// explainSessionStatus's second-to-last string parameter is the root
	// whose "<root>/.rush/locks" subtree holds the session's lock file —
	// historically named cwd because setupApp's --data-dir-aware
	// resolution wasn't wired through here. Pass the already-resolved data
	// directory instead of the raw --cwd value so `sessions why` honors
	// --data-dir / a configured data_directory like `sessions list` /
	// `sessions locks` / `sessions watch` do (task #233 — same
	// cwd-hardcoding bug class as #219/#224/#231).
	return explainSessionStatus(cmd.Context(), a, a.Config().Options.DataDirectory, sess.ID, os.Stdout)
}

// explainSessionStatus writes a terse, plain-text explanation of why the
// session has the status it has. It is the testable core of
// `rush sessions why`: it takes the app services, the resolved data
// directory (for the locks dir), the session id, and an output writer, so
// tests can drive it with a hand-built *app.App and a t.TempDir() without
// spinning up cobra.
//
// dataDir is the rush data directory itself (e.g. what
// a.Config().Options.DataDirectory resolves to — honoring --data-dir / a
// configured data_directory), NOT the project cwd: the lock file lives at
// <dataDir>/locks/session-<id>.lock, one level shallower than the old
// <cwd>/.rush/locks layout this parameter used to assume. See task #233
// (same cwd-hardcoding bug class as #219/#224/#231): the caller used to
// pass the raw --cwd value here, ignoring --data-dir entirely.
//
// The whole explanation is built from the batch facts, the session lock fact
// and the shared session-activity classifier (explainWhyBatch); no legacy
// async_jobs read runs.
func explainSessionStatus(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) error {
	return explainWhyBatch(ctx, a, dataDir, sessionID, out)
}

// describeRunDriver renders the "why" clause naming the live `rush run` loop
// that drives a session, e.g.
//
//	"a `rush run` loop (PID 1234, host 8a3f0c2b alive) drives this session; a reaction is owed and it retries at its next opportunity"
func describeRunDriver(d session.SessionDriver, reactionOwed bool) string {
	clause := fmt.Sprintf("a `rush run` loop (PID %d, host %s %s) drives this session", d.PID, short(d.HostID), strings.ToLower(d.Status.String()))
	if reactionOwed {
		return clause + "; a reaction is owed and it retries at its next opportunity"
	}
	return clause + "; it waits for its scope to close"
}

// finishReasonOrUnknown returns the finish reason string, or "(unknown)"
// when there is no Finish part at all (message never finished).
func finishReasonOrUnknown(f *message.Finish) string {
	if f == nil || f.Reason == "" {
		return "(unknown)"
	}
	return string(f.Reason)
}
