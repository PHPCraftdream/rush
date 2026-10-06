package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
             delegation is still live: a running async_jobs row names a
             DESCENDANT session and its host (the process running it, which
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
// The verdict is the same App.SessionActivity classifier `sessions list`
// reads (one decision, no per-command layers); this function renders it,
// including the "at rest" case. The session's own plain background jobs are
// reported by describeAsyncJobsAndDebt.
//
// The reader takes the lock facts from the App's own data dir, which for a
// real App is the same directory this dataDir parameter carries (the
// resolved --data-dir). A hand-built test App must point itself at its
// fixture directory itself (SetDataDirForTest in the test or a test
// helper): this function used to call it here, mutating the App from
// production code.
func explainSessionStatus(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) error {
	// One classifier verdict is the whole decision (R-ACT): the facts and
	// their reduction live in internal/session + internal/app; this command
	// only renders them. Status words follow the command's established
	// vocabulary; the classifier's Description names the evidence.
	act, actErr := a.SessionActivity(ctx, sessionID)
	if actErr != nil {
		return fmt.Errorf("failed to classify session %s: %w", sessionID, actErr)
	}
	v := act.Verdict
	f := act.Facts

	// Awaiting-answer enrichment (#1158): the pending child_question
	// notices are the durable record the coordinator's in-memory ledger
	// leaves for readers outside its process. Read-only; a failed read
	// changes nothing (the plain verdict stands).
	if store := a.AsyncJobStore(); store != nil {
		if qs, qsErr := store.PendingChildQuestions(ctx, sessionID); qsErr == nil && len(qs) > 0 {
			f.ChildQuestions = qs
			v = session.ClassifySessionActivity(f)
		}
	}

	status := listStatus(v)
	if status == "" {
		status = "at rest"
	}
	// A live verdict over a lock that recorded a dead holder is annotated,
	// not demoted: the PID is evidence, not the Kind (decision 1).
	if status != "at rest" && v.Kind != session.ActivityCrashed &&
		(v.CrashedPID > 0 || f.Lock.Kind == session.LockDead) {
		status += " (stale lock)"
	}

	// Phase 0 stall surfacing (sessions_stall.go): the pulse/last-message
	// lines, and a STALLED header when the heartbeat is stale past the
	// threshold while the holder PID is alive.
	printStallHeader(ctx, a, dataDir, sessionID, status, f.Lock, out)
	fmt.Fprintf(out, "reason: %s.\n", v.Description)
	if f.EndUnreadable {
		fmt.Fprintf(out, "note: the session row could not be read; the end state is unknown.\n")
	}
	describeChildQuestionsForWhy(f, sessionID, out)

	// Sub-agent pulse (in turn only, display only): the lock heartbeat proves
	// the process is alive, not that a delegated sub-agent is making
	// progress; surface the call tree's freshest activity. Baseline is the
	// lock's mtime -- a display timestamp, not a liveness input.
	if v.Kind == session.ActivityInTurn && f.Lock.Kind == session.LockHeld {
		if st, statErr := os.Stat(session.SessionLockPath(dataDir, sessionID)); statErr == nil {
			now := time.Now()
			if note := subAgentActivityNote(ctx, a, sessionID, st.ModTime().Unix(), now); note != "" {
				fmt.Fprintf(out, "%s\n", note)
			}
		}
	}

	// Every verdict names the driver: who runs the reaction loop is part of
	// why the session is (or is not) waiting.
	if f.Driver != nil {
		fmt.Fprintf(out, "driver: %s.\n", describeRunDriver(*f.Driver, f.DriverOwes))
	}

	if v.Kind == session.ActivityCrashed {
		fmt.Fprintf(out, "If this was an unrecovered panic, grep rush.log for %q around the\n", crashLogMarker)
		fmt.Fprintf(out, "time this session's lock went stale — Execute's top-level recover logs\n")
		fmt.Fprintf(out, "the panic and stack trace there before the process exits.\n")
	}

	// Raw last-assistant finish, for context only: it is deliberately NOT a
	// verdict input any more (decision 4 -- ended_reason is the only end
	// signal; a kill -9 between turns leaves no finish and no reason).
	msgs, msgsErr := a.Messages.List(ctx, sessionID)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last assistant message:")
	if msgsErr != nil {
		fmt.Fprintf(out, "  (could not read: %v)\n", msgsErr)
	} else {
		var lastAssistant *message.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				lastAssistant = &msgs[i]
				break
			}
		}
		var finish *message.Finish
		if lastAssistant != nil {
			finish = lastAssistant.FinishPart()
		}
		if lastAssistant == nil {
			fmt.Fprintln(out, "  (none)")
		} else {
			fmt.Fprintf(out, "  finish_reason: %s\n", finishReasonOrUnknown(finish))
			if finish != nil && finish.Reason == message.FinishReasonError {
				errText := finish.Message
				if errText == "" {
					errText = "(error finish reason but no error text stored)"
				}
				fmt.Fprintf(out, "  error:         %s\n", errText)
			}
		}
	}

	describeAsyncJobsAndDebt(ctx, a, sessionID, out)

	return nil
}

// describeChildQuestionsForWhy renders the awaiting-answer section of
// `sessions why` (#1158): one line per pending child_question notice naming
// the child and its question, then WHO must answer. The question is not
// addressed to the CLI operator -- it is addressed to the orchestrating
// agent driving the root session, which answers in-process via the
// `agent` tool (resume_session_id=<child>); only when that loop is gone
// does the operator relay the question by injecting a hint into the root.
func describeChildQuestionsForWhy(f session.ActivityFacts, rootID string, out io.Writer) {
	if len(f.ChildQuestions) == 0 {
		return
	}
	fmt.Fprintln(out)
	for _, q := range f.ChildQuestions {
		fmt.Fprintf(out, "waiting for your answer: child %s asked: %s\n", q.ChildSessionID, q.Question)
		fmt.Fprintf(out, "  answer: the orchestrating agent must call `agent` with resume_session_id=%q and its answer as prompt (the result arrives with delegation %s).\n",
			q.ChildSessionID, q.DelegationToolCallID)
		if f.Driver == nil {
			fmt.Fprintf(out, "  the orchestrating `rush run` loop is not live; to relay the question, run:\n")
			fmt.Fprintf(out, "    rush sessions inject %s \"Sub-agent %s is paused on its question; resume it with agent(resume_session_id=%q) and your answer as prompt.\"\n",
				rootID, q.ChildSessionID, q.ChildSessionID)
		}
	}
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

// describeAsyncJobsAndDebt renders the plain-language jobs/debt section of
// `sessions why` (doc sec.5 step 7): which jobs are running on which host,
// whether a reaction debt is pending, and whether recovery already marked
// something interrupted. Silent (writes nothing) when this App has no
// AsyncJobStore (SkipAgentSetup, or an App built without one in a test).
func describeAsyncJobsAndDebt(ctx context.Context, a *app.App, sessionID string, out io.Writer) {
	store := a.AsyncJobStore()
	if store == nil {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Async jobs:")
	jobs, err := store.ListAsyncJobsForOwner(ctx, sessionID)
	if err != nil {
		fmt.Fprintf(out, "  (could not read: %v)\n", err)
		return
	}
	if len(jobs) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		var running, interrupted int
		for _, j := range jobs {
			switch j.State {
			case "running":
				running++
			case "interrupted":
				interrupted++
			}
		}
		fmt.Fprintf(out, "  %d total, %d running, %d marked interrupted by recovery\n", len(jobs), running, interrupted)
		for _, j := range jobs {
			if j.State != "running" {
				continue
			}
			fmt.Fprintf(out, "  running: tool call %s (%s) on host %s [%s]\n",
				j.ToolCallID, j.Kind, short(j.HostID), strings.ToLower(store.HostLiveness(j.HostID).String()))
		}
	}
	// C12: DUR-4's debt predicate counts a completed job whose notice is still
	// 'pending' (nothing has pulled it into history yet) exactly like one
	// already delivered but unreacted -- reporting only the delivered half
	// showed "none" for a session that still owes the model a turn.
	anyDebt, anyErr := store.ReactionDebtExists(ctx, sessionID)
	visible, visErr := store.VisibleReactionDebtExists(ctx, sessionID)
	switch {
	case anyErr != nil && visErr != nil:
		// Both reads failed: say nothing rather than a wrong "none".
	case visErr == nil && visible:
		fmt.Fprintln(out, "  reaction debt: pending — a completed job's result is in history but no model turn has reacted to it yet")
	case anyErr == nil && anyDebt:
		fmt.Fprintln(out, "  reaction debt: pending — a completed job's result has not been delivered into history yet, and no model turn has reacted to it")
	case anyErr == nil:
		fmt.Fprintln(out, "  reaction debt: none")
	}
}

// finishReasonOrUnknown returns the finish reason string, or "(unknown)"
// when there is no Finish part at all (message never finished).
func finishReasonOrUnknown(f *message.Finish) string {
	if f == nil || f.Reason == "" {
		return "(unknown)"
	}
	return string(f.Reason)
}
