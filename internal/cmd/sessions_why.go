package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

This is the command to reach for when "sessions list" shows a session as
"crashed" and you want to know whether it genuinely died mid-turn or
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
             session is waiting on its own job, so it is NOT done.
  delegating — this session's own session lock is gone (or stale) but a
             delegation is still live: a running async_jobs row names a
             DESCENDANT session and its host (the process running it, which
             holds a per-process host lock) is alive or cannot be probed.
             Delegated sub-agent work is still in progress, so the session
             is NOT done even though its own turn yielded.
  at rest  — no session lock file. Not running, not crashed, no live
             delegation and no running background job of its own.

When the raw lock signal says "crashed" but the last assistant message
finished cleanly (end_turn), the verdict says so explicitly and treats
the session as done — this is the same reclassification "sessions list"
applies via reclassifyCrashedAsDone, surfaced here in plain language.
That reclassification is suppressed while a delegation is live (a running
delegation row on a host that is not provably dead) or the session's own
background job is running: the end_turn is the session's own yield, not
completion.`,
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
// It deliberately mirrors the two-step status computation that
// `sessions list` uses (computeSessionStatuses → reclassifyCrashedAsDone)
// but for a single session, and adds the "at rest" case those helpers
// don't represent (they only return entries for sessions that HAVE a lock).
//
// The descendant check (AsyncJobStore.LiveDescendantJobs) is the cross-process
// layer `sessions list` applies through markDelegatingLiveDescendants: a
// session whose own per-turn lock was released is NOT done while a live
// delegation row (on a host not provably dead) names a descendant session.
// It is computed before the verdict switch so every branch can consult it.
// Only delegation rows count: the session's own plain background jobs are
// reported by describeAsyncJobsAndDebt, not by this verdict.
func explainSessionStatus(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) error {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to list messages for session %s: %w", sessionID, err)
	}

	// Lock state — same path / parse logic as computeSessionStatuses and
	// sessionsLocksCmdRun, but for the single session we care about.
	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	var (
		hasLock          bool
		statFailed       bool
		statFailure      error
		pid              int
		pidAlive         bool
		pidBoundExceeded bool
		pidGenuinelyDead bool
		heartAge         time.Duration
	)
	if st, statErr := os.Stat(lockPath); statErr == nil {
		hasLock = true
		pid = session.ReadLockPID(lockPath)
		heartAge = time.Since(st.ModTime())
		// A CONFIRMED-dead PID (pid > 0 but IsProcessAlive is false) is a
		// trustworthy signal regardless of heartbeat age — that specific
		// process really doesn't exist. But pid <= 0 is ambiguous, not
		// necessarily dead: on Windows, tryLockFile's mandatory, whole-file
		// LockFileEx lock means reading the PID from another process fails
		// for the holder's ENTIRE lifetime (session.readLockFile's Windows
		// note), so pid == 0 is the norm for a live session there, not a
		// sign of death. Only in that unreadable case do we fall back to
		// heartbeat freshness.
		//
		// A CONFIRMED-alive PID is trusted only within
		// session.MaxPidFallbackAge of the lock's mtime — past that, the
		// lock is old enough that no genuinely healthy holder could still
		// be running it, so a currently-alive PID is far more likely an OS
		// PID-reuse coincidence than the original holder (task #250, the
		// same fix tasks #235/#241 already applied to InspectSessionLock,
		// sessions_watch.go and computeSessionStatuses). Without this bound,
		// `sessions why` would say "running / lock held by live PID N" for
		// the very same session `sessions list` already correctly reports as
		// crashed/done — directly contradicting the verdict this command
		// exists to explain.
		switch {
		case pid > 0 && time.Since(st.ModTime()) >= session.MaxPidFallbackAge:
			// Age bound forced pidAlive=false even though the PID number may
			// currently belong to a live, unrelated process (OS reuse). Track
			// this so the reason text below doesn't claim a factually-alive PID
			// "is not alive" — the bound from #250 fixed the verdict but left
			// the explanation text factually wrong in exactly the reuse case
			// (task #256).
			pidBoundExceeded = true
			pidAlive = false
			// The verdict itself doesn't change here — a lock this old is
			// treated as dead regardless — but which REASON TEXT is accurate
			// still depends on whether the recorded PID is genuinely dead or
			// genuinely reused. Checking IsProcessAlive is negligible cost for
			// a one-shot diagnostic command, and without it every old lock
			// (including the dominant "process crashed hours ago, PID really
			// is dead" case) incorrectly printed "likely OS PID reuse" (#257).
			pidGenuinelyDead = !session.IsProcessAlive(pid)
		case pid > 0:
			pidAlive = session.IsProcessAlive(pid)
		default:
			pidAlive = heartAge <= session.LockStaleDuration
		}
	} else if !os.IsNotExist(statErr) {
		statFailed = true
		statFailure = statErr
	}

	// Last assistant message + its finish part (if any). Same
	// reverse-scan as reclassifyCrashedAsDone.
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

	// Descendant work: this session's own lock says nothing about work
	// still running in a DESCENDANT session. A parent that delegated to a
	// sub-agent releases its per-turn lock the moment it yields, so the
	// signal that the workflow is still alive lives on live async_jobs
	// delegation rows (child_session_id), each checked against its owning
	// host's liveness — visible to any process. Same walk `sessions list`
	// applies through markDelegatingLiveDescendants; see
	// session.AsyncJobStore.LiveDescendantJobs.
	//
	// Own jobs: the same holds for the session's OWN running plain job
	// (bash/run_command) -- LiveDescendantJobs only sees delegation rows, so
	// a root waiting on its own job would read as done/at rest. Same
	// promotion `sessions list` applies through markRunningOwnJobs.
	var (
		liveDescendants []session.LiveJob
		walkIncomplete  bool
		ownJobs         []session.LiveJob
		ownIncomplete   bool
	)
	if store := a.AsyncJobStore(); store != nil {
		liveDescendants, walkIncomplete = store.LiveDescendantJobs(ctx, sessionID)
		ownJobs, ownIncomplete = store.LiveOwnJobs(ctx, sessionID)
	}
	descendantCaveat := ""
	if walkIncomplete && len(liveDescendants) == 0 {
		// A failed child listing means the tree could not be fully
		// enumerated, so a terminal verdict below is only as trustworthy
		// as that enumeration. Say so instead of asserting done flatly.
		descendantCaveat = "WARNING: could not enumerate this session's descendant sessions; a live delegation may exist that this check could not see.\n"
	}
	if ownIncomplete && len(ownJobs) == 0 {
		descendantCaveat += "WARNING: could not read this session's own async jobs; a running job may exist that this check could not see.\n"
	}

	// Verdict + reason text. The cases match the Long help above; "at
	// rest" (no lock) is the one computeSessionStatuses can't express.
	switch {
	case statFailed:
		fmt.Fprintf(out, "status: unknown (could not verify)\n")
		fmt.Fprintf(out, "reason: could not inspect lock file (%s): %v — cannot confirm running, crashed, or at rest.\n", lockPath, statFailure)
	case !hasLock:
		if len(liveDescendants) > 0 {
			// The session itself is at rest, but delegated sub-agent work
			// is still in progress — NOT done. Same promotion
			// markDelegatingLiveDescendants applies to `sessions list`.
			fmt.Fprintf(out, "status: delegating\n")
			fmt.Fprintf(out, "reason: no lock file present for this session, but %s — delegated sub-agent work is still in progress, so this session is NOT done.\n",
				describeLiveDescendants(liveDescendants))
			if finish != nil && finish.Reason == message.FinishReasonEndTurn {
				fmt.Fprintf(out, "the last assistant message's end_turn is this session's own yield before the delegation, not completion.\n")
			} else if lastAssistant == nil {
				fmt.Fprintf(out, "no assistant message recorded yet.\n")
			} else {
				fmt.Fprintf(out, "last assistant message did not finish cleanly (%s).\n", finishReasonOrUnknown(finish))
			}
		} else if len(ownJobs) > 0 {
			// At rest as far as locks go, but the session is waiting on its
			// OWN running background job -- NOT done.
			fmt.Fprintf(out, "status: running\n")
			fmt.Fprintf(out, "reason: no lock file present for this session (idle between turns), but %s — the session is waiting on it, so it is NOT done.\n",
				describeLiveOwnJobs(ownJobs))
		} else {
			fmt.Fprintf(out, "status: at rest\n")
			fmt.Fprintf(out, "reason: no lock file present — not running, not crashed.\n")
			if finish != nil && finish.Reason == message.FinishReasonEndTurn {
				fmt.Fprintf(out, "last assistant message finished cleanly (end_turn); session is idle.\n")
			} else if lastAssistant == nil {
				fmt.Fprintf(out, "no assistant message recorded yet.\n")
			} else {
				fmt.Fprintf(out, "last assistant message did not finish cleanly (%s).\n", finishReasonOrUnknown(finish))
			}
			if descendantCaveat != "" {
				fmt.Fprint(out, descendantCaveat)
			}
		}
	case hasLock && pidAlive:
		fmt.Fprintf(out, "status: running\n")
		if pid > 0 {
			fmt.Fprintf(out, "reason: lock held by live PID %d (heartbeat %s old).\n", pid, formatDurationShort(heartAge))
		} else {
			// PID unreadable (normal on Windows while the holder is alive —
			// see the Windows note above) but the heartbeat is fresh.
			fmt.Fprintf(out, "reason: lock held (PID unreadable while active — normal on Windows); heartbeat %s old.\n", formatDurationShort(heartAge))
		}
		// Sub-agent pulse: the lock heartbeat only proves the orchestrator
		// process is alive — if it's blocked inside an `agent` delegation the
		// heartbeat keeps ticking whether the sub-agent is working or hung.
		// Surface the call tree's freshest activity (baseline = heartbeat
		// mtime) so "working" vs "stuck" is distinguishable. See
		// sessions_activity.go.
		now := time.Now()
		lockMtimeUnix := now.Add(-heartAge).Unix()
		if note := subAgentActivityNote(ctx, a, sessionID, lockMtimeUnix, now); note != "" {
			fmt.Fprintf(out, "%s\n", note)
		}
		if finish != nil {
			fmt.Fprintf(out, "last assistant finish: %s\n", finishReasonOrUnknown(finish))
		} else {
			fmt.Fprintf(out, "last assistant finish: (none yet — turn in progress)\n")
		}
	default:
		// hasLock && !pidAlive → raw signal is "crashed". Decide whether
		// the message store contradicts that (clean end_turn → really
		// "done") — this is the reclassifyCrashedAsDone rule.
		//
		// The FIRST-LINE verdict must match what `sessions list` shows:
		// a clean end_turn is reclassified as "done" there, so we emit
		// "status: done (stale lock)" — NOT "status: crashed" — otherwise
		// any orchestrator parsing the first line gets the OPPOSITE verdict
		// from `sessions list` for the same session. The genuinely-crashed
		// case (no clean finish) stays "status: crashed".
		//
		// One exception to the list-parity rule, in the same direction list
		// itself takes it: when a DESCENDANT session still holds a live
		// lock, neither command reports done — the end_turn is the
		// parent's own yield before the delegation, not completion (see
		// markDelegatingLiveDescendants). A crashed parent with a live
		// child still reports "crashed": that is the parent's own dead
		// holder, and the two statuses must stay distinct.
		// Pick the "why dead" phrasing by cause:
		//
		//   - pid <= 0 (unreadable, e.g. normal on Windows while the
		//     holder is alive — see the Windows note above): there was
		//     never a "PID 0" holder to declare dead. The real evidence is
		//     a stale heartbeat, so say that instead of naming a fictional
		//     PID (task #258).
		//   - pidBoundExceeded && !pidGenuinelyDead: the lock is old but
		//     the recorded PID number currently belongs to a live,
		//     unrelated process — genuine OS PID-reuse territory. Include
		//     both the bound threshold and the lock's actual age so the
		//     operator can see both numbers (task #257 review nit).
		//   - otherwise (includes pidBoundExceeded && pidGenuinelyDead,
		//     which reads identically to the plain unbounded case — no
		//     need to speculate about PID reuse once IsProcessAlive has
		//     already confirmed the PID is dead, task #257): a genuinely-dead
		//     PID really isn't alive; claiming otherwise would contradict
		//     what tasklist/ps shows the operator — the very contradiction
		//     task #250's verdict fix was meant to remove, caught
		//     surviving here in the explanation text (#256).
		var holderDeadReason string
		switch {
		case pid <= 0:
			holderDeadReason = fmt.Sprintf("lock file exists but its PID could not be read and the heartbeat is stale (%s old, exceeds LockStaleDuration) — the recorded holder cannot be confirmed alive", formatDurationShort(heartAge))
		case pidBoundExceeded && !pidGenuinelyDead:
			holderDeadReason = fmt.Sprintf("lock file is older than MaxPidFallbackAge (%s, lock is %s old); its recorded PID %d is no longer trustworthy (likely OS PID reuse) — treating the holder as dead", formatDurationShort(session.MaxPidFallbackAge), formatDurationShort(heartAge), pid)
		default:
			holderDeadReason = fmt.Sprintf("lock file exists but holder PID %d is not alive", pid)
		}
		if finish != nil && finish.Reason == message.FinishReasonEndTurn {
			if len(liveDescendants) > 0 {
				// The reclassification to "done" is suppressed: the
				// session's own turn yielded (end_turn) but delegated
				// sub-agent work is still live, so the session is NOT
				// done. This is the observed production bug — a stale or
				// released per-turn lock being read as completion while
				// an implementation sub-agent was still working.
				fmt.Fprintf(out, "status: delegating (stale lock)\n")
				fmt.Fprintf(out, "reason: %s; %s — the end_turn above is this session's own yield before the delegation, so the session is NOT done.\n",
					holderDeadReason, describeLiveDescendants(liveDescendants))
			} else if len(ownJobs) > 0 {
				// Same suppression for the session's own running job: the
				// end_turn is its yield while the job runs, not completion.
				fmt.Fprintf(out, "status: running (stale lock)\n")
				fmt.Fprintf(out, "reason: %s; %s — the end_turn above is this session's own yield while the job runs, so the session is NOT done.\n",
					holderDeadReason, describeLiveOwnJobs(ownJobs))
			} else {
				fmt.Fprintf(out, "status: done (stale lock)\n")
				fmt.Fprintf(out, "reason: %s; last assistant message finished cleanly (end_turn).\n", holderDeadReason)
				fmt.Fprintf(out, "\n")
				fmt.Fprintf(out, "NOTE: this matches the reclassification \"sessions list\" applies via\n")
				fmt.Fprintf(out, "reclassifyCrashedAsDone — likely a stale lock from a process that exited\n")
				fmt.Fprintf(out, "without cleanup, or another process finished this session concurrently.\n")
				fmt.Fprintf(out, "Treat as done.\n")
				if descendantCaveat != "" {
					fmt.Fprint(out, descendantCaveat)
				}
			}
		} else {
			fmt.Fprintf(out, "status: crashed\n")
			fmt.Fprintf(out, "reason: %s.\n", holderDeadReason)
			if lastAssistant == nil {
				fmt.Fprintf(out, "no assistant message with a clean finish — likely died mid-turn.\n")
			} else {
				fmt.Fprintf(out, "no clean finish found (last finish: %s) — likely died mid-turn.\n", finishReasonOrUnknown(finish))
			}
			fmt.Fprintf(out, "If this was an unrecovered panic, grep rush.log for %q around the\n", crashLogMarker)
			fmt.Fprintf(out, "time this session's lock went stale — Execute's top-level recover logs\n")
			fmt.Fprintf(out, "the panic and stack trace there before the process exits.\n")
		}
	}

	// Always surface the raw last-assistant finish reason + error text if
	// present, so the operator sees the underlying signal regardless of
	// which branch above fired.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last assistant message:")
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

	describeAsyncJobsAndDebt(ctx, a, sessionID, out)

	return nil
}

// describeLiveDescendants renders the "why" clause naming every descendant
// session a live async_jobs delegation row points at, e.g.
//
//	"descendant session 8a3f0c2b has live work (delegated by 1234abcd, tool call call-1, host xyz alive)"
//
// so the operator can see WHICH sub-agent is keeping the session alive, WHO
// delegated to it, and on which host. Multiple live descendants are listed
// together rather than collapsing to a count — the whole point of the
// command is to name the evidence.
func describeLiveDescendants(live []session.LiveJob) string {
	items := make([]string, 0, len(live))
	for _, j := range live {
		items = append(items, fmt.Sprintf("%s (delegated by %s, tool call %s, host %s %s)",
			short(session.HashID(j.ChildSessionID)), short(session.HashID(j.SessionID)),
			j.ToolCallID, short(j.HostID), strings.ToLower(j.HostStatus.String())))
	}
	if len(items) == 1 {
		return "descendant session " + items[0] + " has live work"
	}
	return "descendant sessions " + strings.Join(items, ", ") + " still have live work"
}

// describeLiveOwnJobs renders the "why" clause naming the session's own
// running plain jobs, e.g.
//
//	"its own background job call-1 (command, host xyz alive) is still running"
func describeLiveOwnJobs(live []session.LiveJob) string {
	items := make([]string, 0, len(live))
	for _, j := range live {
		items = append(items, fmt.Sprintf("%s (%s, host %s %s)",
			j.ToolCallID, j.Kind, short(j.HostID), strings.ToLower(j.HostStatus.String())))
	}
	if len(items) == 1 {
		return "its own background job " + items[0] + " is still running"
	}
	return "its own background jobs " + strings.Join(items, ", ") + " are still running"
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
