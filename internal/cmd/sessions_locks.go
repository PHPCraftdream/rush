package cmd

// The `sessions locks` subcommand: list session lock files with heartbeat
// pulse classification, plus the --prune path guarded by the
// lockHolderProvablyDead OS-level probe.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsLocksCmd = &cobra.Command{
	Use:   "locks",
	Short: "List active session lock files",
	Long: `Scan the .rush/locks directory for session lock files and report
their status: session id, PID, when the lock was acquired, and whether
it appears stale (process not running or lock older than 10 minutes).

Lock files are typically acquired when a session is running and released
when the run completes. Stale locks can accumulate if processes crash
without cleanup. By default this command is READ-ONLY: it lists locks and
never removes anything — an empty lock file with no held OS lock is
harmless (the next acquirer reopens and overwrites it; see
internal/session/lock.go's Release), so auto-deleting stable per-session
lock files is unnecessary and only ever reintroduces TOCTOU "unlink a path
a live holder may be reusing" bugs.

Pass --prune to opt in to removing a lock once it has been proven (via a
real OS-level lock probe, not just its mtime age) that no live process
actually holds it. Entries that merely LOOK stale by mtime (older than 60s)
but that a real OS-level lock probe proves are still held are never deleted
— only a lock file with no live OS-level holder is removed under --prune.
See lockHolderProvablyDead's doc comment for the full discipline and its
two documented, narrow residual windows (a post-probe TOCTOU on removal,
and brief probe-induced contention with a concurrent "rush run" starting
on the exact same session id). --prune is opt-in precisely because those
narrow windows are accepted for an explicit operator request, not for a
default that runs silently on every invocation.

Use --stale-only to filter to suspicious locks. Use --json for NDJSON
output suitable for metrics collection or automation.`,
	Example: `
# Show all locks
rush sessions locks

# Show only stale locks
rush sessions locks --stale-only

# Stream to jq for scripting
rush sessions locks --json | jq '.session_id'
  `,
	RunE: sessionsLocksCmdRun,
}

// lockPulseStatus classifies a lock file by its heartbeat mtime.
// Heartbeat interval = 10s, stale threshold = 20s (session.lockStaleDuration).
//
//	0–10s  → "alive"    (fresh heartbeat)
//	10–15s → "ping"     (one beat overdue, likely OK)
//	15–20s → "stopping" (two beats missed, probably finishing)
//	>20s   → "offline"  (stale — holder crashed or exited without Release)
func lockPulseStatus(ageSec int64) string {
	switch {
	case ageSec <= 10:
		return "alive"
	case ageSec <= 15:
		return "ping"
	case ageSec <= 20:
		return "stopping"
	default:
		return "offline"
	}
}

// lockHolderProvablyDead decides, via a real OS-level lock attempt, whether
// the lock file at lockPath under dataDir/locks/session-<id>.lock may safely
// be auto-deleted. Mirrors sessions_kill.go's probeThenKillHolder — same
// "don't trust mtime alone, prove it via the real OS lock" discipline (task
// #222 hardening).
//
// Why mtime alone stopped being safe here: task #214/#222 gated the
// heartbeat's mtime-touch on real RecordActivity() calls instead of an
// unconditional 10s timer. A session blocked on a single long-running tool
// call (bounded by toolExecutionMaxDefault, up to 45 minutes) can now go
// well past the old 60s auto-delete threshold with zero recorded activity
// while still being completely healthy and still holding the real OS lock.
// Unconditionally os.Remove-ing the path in that case does NOT revoke the
// live holder's flock/LockFileEx (advisory locks are bound to the inode,
// not the path) — it just lets a SECOND process create a fresh inode at the
// same path and believe it owns the session, producing two simultaneous
// owners of one session id (see the package doc on session.SessionLock).
//
// Returns true only when TryAcquireSessionLock itself succeeds — i.e. we
// just proved, at the kernel level, that nobody holds the lock right now.
// The lock we acquired to prove that is released immediately. Any other
// outcome (busy error, or an unidentified failure) is treated as "do not
// delete" — the conservative default, since a false "provably dead" is what
// causes the two-owners bug, while a false "still alive" merely means the
// stale entry lingers one more `sessions locks` invocation.
//
// What this does NOT solve (task #230, narrower follow-ups to #222): two
// related gaps remain, both real but far narrower than the unconditional
// os.Remove this function replaced.
//
//  1. Residual TOCTOU between probe and removal. This function proves
//     "nobody holds the lock" and releases immediately; it does not itself
//     remove the file. Its caller (sessionsLocksCmdRun) calls os.Remove
//     afterward, as a separate, non-atomic step. In the gap between this
//     function's Release() and the caller's os.Remove(), a fresh
//     `rush run --session <id>` can legitimately re-acquire the same
//     session id and start writing — and the caller would then unlink
//     that new, live holder's lock file, reproducing (in a window on the
//     order of a syscall or two, not the original unbounded-mtime window)
//     the exact two-owners scenario this hardening exists to prevent.
//     Closing this fully would mean holding the OS lock across the
//     os.Remove itself (return the still-held *session.SessionLock to the
//     caller instead of releasing here, remove the path while holding it,
//     then release). That was deliberately NOT done: this package's sibling
//     session.FileLock.Release doc comment already documents why removing a
//     path while a lock on it is held is cross-platform-fragile — POSIX
//     unlink of a locked-but-open file is harmless (flock is keyed off the
//     inode), but the lock files here are opened without FILE_SHARE_DELETE
//     on Windows, so a concurrent opener's handle can make the delete fail
//     with a sharing violation, or a successful delete can race a
//     subsequent opener into creating a distinct file object at the same
//     path while an older locked one still exists — i.e. the fix risks
//     reintroducing a shaped version of the same two-owners bug on Windows
//     specifically, in exchange for closing a gap that is already down to
//     single-digit milliseconds. Not worth it here; the window is accepted
//     and documented instead.
//  2. Probe-induced transient contention. Proving death takes a real
//     exclusive OS lock (open + LockFileEx/flock + truncate + PID write +
//     Sync + sidecar write + Chtimes, then release) on every lock file
//     `sessions locks` inspects that looks older than autoDeleteAfter. A
//     `rush run` legitimately starting on that exact session id during
//     that brief window gets a hard "already in use" abort with no retry.
//     This is why the fix for (1) above is not simply "hold the lock
//     longer" — extending how long the probe holds the OS lock would widen
//     this exact contention window, trading one narrow risk for a wider
//     one. Given how narrow the window already is (a single acquire+release
//     cycle, not the run's full lifetime) and that it only collides with a
//     `rush run` starting on the SAME session id in that SAME instant,
//     this is accepted as-is rather than gated behind a flag — see
//     sessionsLocksCmdRun's doc comment for the fuller default-behavior
//     tradeoff.
func lockHolderProvablyDead(dataDir, sessionID string) bool {
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		// Busy (a real holder exists) or an unidentified probe failure —
		// neither is proof of death. Do not delete.
		return false
	}
	_ = lk.Release()
	return true
}

// preAutoDeleteRemoveHook is a test-only seam for sessionsLocksCmdRun's
// auto-delete branch. When non-nil, it is called synchronously between
// lockHolderProvablyDead proving a holder dead and the os.Remove call,
// allowing a test to deterministically reproduce the TOCTOU window where a
// concurrent process removes the lock file between the probe and the Remove.
// Always nil in production.
var preAutoDeleteRemoveHook func(lockPath string)

func sessionsLocksCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	staleOnly, _ := cmd.Flags().GetBool("stale-only")
	prune, _ := cmd.Flags().GetBool("prune")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	// Use the data directory setupApp already resolved onto `a` (honors
	// --data-dir and the project's configured data_directory) instead of
	// recomputing a cwd-based guess — see task #219/#224, and task #231
	// (this exact function was left unfixed when those landed).
	dataDir := a.Config().Options.DataDirectory
	locksDir := filepath.Join(dataDir, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		if os.IsNotExist(err) {
			if asJSON {
				return nil
			}
			fmt.Println("(no locks)")
			return nil
		}
		return err
	}

	type lockItem struct {
		SessionID   string `json:"session_id"`
		PID         int    `json:"pid"`
		PulseSec    int64  `json:"pulse_sec"`
		Pulse       string `json:"pulse"` // alive / ping / stopping / offline
		AcquiredAt  int64  `json:"acquired_at_unix"`
		DurationSec int64  `json:"duration_seconds"`
		Stale       bool   `json:"stale"`
		BudgetSec   int64  `json:"budget_sec,omitempty"` // --timeout seconds, 0 if not set
		// SubAgent, when non-empty, means the freshest activity in this
		// session's call tree came from an in-flight sub-agent delegation,
		// NOT the top-level heartbeat — so PulseSec/Pulse below are the
		// sub-agent's activity age, which is the honest "is anything actually
		// making progress" signal an operator wants during a long delegation.
		SubAgent string `json:"sub_agent,omitempty"`
	}

	var locks []lockItem
	now := time.Now()
	const autoDeleteAfter = 60 * time.Second

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}

		sessionID := strings.TrimPrefix(entry.Name(), "session-")
		sessionID = strings.TrimSuffix(sessionID, ".lock")

		info, _ := entry.Info()
		age := now.Sub(info.ModTime())

		lockPath := filepath.Join(locksDir, entry.Name())

		// Auto-delete candidate: mtime older than 1 minute AND the operator
		// explicitly opted in via --prune. By default `sessions locks` is
		// READ-ONLY — it never removes anything. Deleting stable per-session
		// lock files is unnecessary in the first place (an empty lock file
		// with no held OS lock is harmless: the next acquirer reopens and
		// overwrites it, see internal/session/lock.go's Release), so the
		// whole class of TOCTOU "unlink a path that a live holder may be
		// reusing" bugs is closed by NOT auto-deleting unless an operator
		// asks for it.
		//
		// mtime alone is NO LONGER proof of death (task #222) — the
		// heartbeat's mtime touch is gated on RecordActivity, so a session
		// blocked on a single long-running tool call (up to
		// toolExecutionMaxDefault, 45 minutes) can look stale here while
		// still being completely healthy and still holding the real OS lock.
		// Before deleting under --prune, prove it via a real OS-level lock
		// attempt (lockHolderProvablyDead) — same discipline as
		// sessions_kill.go's probeThenKillHolder. Unlinking a path out from
		// under a live holder's flock/LockFileEx does not revoke it (advisory
		// locks are bound to the inode, not the path); it just lets a second
		// process create a fresh inode at the same path and believe it owns
		// the session — two owners of one session id.
		if prune && age > autoDeleteAfter {
			if lockHolderProvablyDead(dataDir, sessionID) {
				if preAutoDeleteRemoveHook != nil {
					preAutoDeleteRemoveHook(lockPath)
				}
				err := os.Remove(lockPath)
				switch {
				case err == nil, errors.Is(err, fs.ErrNotExist):
					// errors.Is(err, fs.ErrNotExist) (task #244): a concurrent
					// process — `sessions reap`, `sessions kill`, `sessions
					// reset --force`, or another parallel `sessions locks`
					// racing the same auto-delete path — may have already
					// removed this exact file between lockHolderProvablyDead's
					// probe above and this Remove call. The goal of this
					// Remove ("the stale lock file is gone from disk") is
					// already achieved in that case; it is not a failure.
					// Treating ENOENT as an error here (as task #234's fix
					// briefly did) produced a false "warning: could not
					// remove..." on stderr plus a phantom display-path row
					// below for a file that no longer exists (PID 0, offline,
					// age computed from the stale cached entry.Info()) —
					// exactly the false positive an orchestrating script
					// polling `sessions locks` alongside a concurrent
					// `sessions kill` would hit. Report success and move on,
					// same as the pre-#234 behavior for this specific case.
					fmt.Fprintf(os.Stderr, "removed stale lock %s (age %ds, holder provably dead)\n", entry.Name(), int(age.Seconds()))
					continue
				default:
					// Do NOT silently swallow this (task #234). A failed
					// Remove here is not a harmless no-op: lockHolderProvablyDead's
					// own acquire+release probe (see its doc comment) truncates
					// the lock file's content and freshens its mtime as part of
					// proving death, moments before this Remove runs. If the
					// Remove then fails for a REAL reason (e.g. a Windows
					// sharing-violation window from a concurrent opener that
					// still holds the file open, or permission denied — NOT
					// the file already being gone, which is handled above),
					// the file is left behind with a wiped PID AND a fresh
					// mtime — which every PID-fallback/mtime-based liveness
					// consumer below (and in isSessionLockAlive /
					// InspectSessionLock / computeSessionStatuses) would read
					// as LIVE for the next heartbeat-stale window, even though
					// this probe JUST proved the holder dead. Silently
					// `continue`-ing also made the entry vanish from this
					// listing entirely, so an operator had zero signal
					// anything was wrong. Surface the failure, then fall
					// through to the normal display path below instead of
					// `continue`-ing: age is already > autoDeleteAfter (60s),
					// which lockPulseStatus always classifies as "offline"
					// (>20s), so the entry is correctly shown as stale/offline
					// rather than silently dropped from the listing while its
					// stale file lingers on disk.
					fmt.Fprintf(os.Stderr, "warning: could not remove provably-dead lock %s: %v\n", entry.Name(), err)
				}
			}
			// mtime looks stale but the real OS lock is still held (or the
			// probe was inconclusive), or the provably-dead lock's removal
			// itself failed — do NOT delete (or it's already gone from disk
			// in the success case above, which already `continue`d). Fall
			// through and display it like any other lock; its Pulse will
			// read "offline" so operators still see it's not heartbeating,
			// without risking a second process reclaiming a session that is
			// still alive.
		}

		pulseSec := int64(age.Seconds())
		pulse := lockPulseStatus(pulseSec)
		stale := pulse == "offline"

		// Call-tree pulse override: the lock mtime only ticks on real
		// activity the heartbeat goroutine's RecordActivity gate observed
		// (stream chunks — task #300), NOT on tool-call execution itself.
		// A session can be genuinely busy running ordinary top-level tool
		// calls (view, todos, ...) between model responses, with no stream
		// chunk arriving for well over the heartbeat's stale threshold —
		// the heartbeat mtime then reads "offline" for a live, working
		// session (task #321; observed live: PULSE_AGE == ELAPSED == 36s
		// with the process alive and real tool calls in progress).
		// computeCallTreeActivity's LatestUnix already covers the ROOT
		// session's own message activity (created_at/updated_at, bumped on
		// every tool-input-start/tool-call/tool-result), not just a
		// descendant sub-agent's — so this override no longer requires
		// act.SubAgentActive to apply. It's still used below to decide
		// whether to show a specific sub-agent's hash: a fresher signal
		// that came from the session's OWN activity, not a delegation,
		// gets no such label.
		var subAgentLabel string
		if act, fresher := callTreeActivityFresherThan(cmd.Context(), a, sessionID, info.ModTime().Unix()); fresher {
			if subAge, ok := act.Age(now); ok {
				pulseSec = int64(subAge.Seconds())
				pulse = lockPulseStatus(pulseSec)
				stale = pulse == "offline"
				if act.SubAgentActive {
					subAgentLabel = short(session.HashID(act.DeepestSessionID))
				}
			}
		}

		if staleOnly && !stale {
			continue
		}

		// session.ReadLockPID (not a raw os.ReadFile+Sscanf) so a live
		// holder's PID is still readable on Windows, where the holder's
		// mandatory LockFileEx range-lock can make the lock file's own
		// content unreadable to this process — see readLockFile's sidecar
		// fallback (internal/session/lock.go) and task #231.
		pid := session.ReadLockPID(lockPath)
		budgetSec := session.ReadLockTimeoutSec(lockPath)

		// Approximate acquire time: mtime when pulse was fresh.
		// For alive locks mtime ≈ now, so we use file birthtime via stat
		// if available; otherwise mtime is the best proxy.
		acqTime := info.ModTime().Unix()
		duration := int64(now.Sub(info.ModTime()).Seconds())

		locks = append(locks, lockItem{
			SessionID:   sessionID,
			PID:         pid,
			PulseSec:    pulseSec,
			Pulse:       pulse,
			AcquiredAt:  acqTime,
			DurationSec: duration,
			Stale:       stale,
			BudgetSec:   budgetSec,
			SubAgent:    subAgentLabel,
		})
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, lock := range locks {
			if err := enc.Encode(lock); err != nil {
				return err
			}
		}
		return nil
	}

	if len(locks) == 0 {
		if staleOnly {
			fmt.Println("(no stale locks)")
		} else {
			fmt.Println("(no locks)")
		}
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION_ID\tPID\tPULSE\tPULSE_AGE\tELAPSED\tBUDGET\tSUB-AGENT")
	for _, lock := range locks {
		budget := "∞"
		if lock.BudgetSec > 0 {
			budget = formatDurationShort(time.Duration(lock.BudgetSec) * time.Second)
		}
		subAgent := "-"
		if lock.SubAgent != "" {
			subAgent = lock.SubAgent
		}
		fmt.Fprintf(
			tw, "%s\t%d\t%s\t%ds ago\t%s\t%s\t%s\n",
			truncate(lock.SessionID, 28),
			lock.PID,
			lock.Pulse,
			lock.PulseSec,
			formatDurationShort(time.Duration(lock.DurationSec)*time.Second),
			budget,
			subAgent,
		)
	}
	return tw.Flush()
}
