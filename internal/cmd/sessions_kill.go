package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <id>",
	Short: "Kill the process holding a session's lock and remove the lock file",
	Long: `Force-release a session that is stuck behind a live or orphan
rush process. Reads the holder PID from .rush/locks/session-<id>.lock,
forcibly kills it (SIGKILL on POSIX, taskkill /F /T on Windows so the
whole subprocess tree dies), waits for the OS to release the file
handle, then removes the lock file.

Use this when:
- A "rush run --session <id>" reports "session is already in use", but
  you know the real holder is dead (or stuck) and won't release.
- "rush sessions reset --force" cannot proceed because the lock survived.
- A previous run was force-killed (TaskStop / Ctrl+C on a wrapper) and
  left the child rush process orphaned, still holding the lock.

On Windows the kill goes through ` + "`taskkill /F /T /PID`" + ` which
also terminates every child the rush process spawned (typically the
external CLI: claude.cmd → node.exe). The plain os.Process.Kill() goes
through OpenProcess(PROCESS_TERMINATE), which can fail with "Access is
denied" for processes launched under Git Bash or MSYS — taskkill avoids
that whole class of issue.

On Unix (Linux/macOS) a CLI-provider child (the ` + "`claude`" + `/` + "`gemini`" + `/` + "`codex`" + `/` + "`qwen`" + ` process rush
launched) is spawned as the leader of its OWN process group, deliberately
separate from rush's, so an ordinary signal to rush does not also kill
it (see internal/session/track_unix.go). Because of that, SIGKILL to the
rush PID by itself does NOT reach that child tree on Unix.

This command closes that gap, but ONLY when it can prove the process
group it is about to signal is still the one that was actually spawned
for the holder just confirmed dead. When rush registers a CLI-provider
child group (automatic for every model run through the ` + "`cli`" + `
provider), the registration is written next to the session's own lock
file, under its per-user ` + "`.rush/locks`" + ` directory (see
internal/session/childgroup_registry_unix.go) -- NOT a shared, predictable
path -- and it is stamped with the exact lock generation token that was
current at registration time (the same generation mechanism
internal/session/lock.go already uses to guard its own destructive
cleanup). Once the rush PID itself is confirmed dead -- whether this
command killed it, or it had already crashed/exited on its own before
this command ever ran -- this command:

  1. sweeps only entries whose recorded generation EXACTLY matches the
     token captured at the moment this specific holder was proven dead --
     read from a live holder's lock right before it was killed, or, for a
     holder that had already crashed, read before this command's own probe
     could acquire the (now-free) lock and overwrite that token. This
     value is fixed once and never re-read afterwards, so a brand-new
     owner acquiring the session in the window right after this command's
     kill (or right after it finds the lock already free) cannot silently
     redirect the sweep at their own live child group. A registered group
     whose generation does NOT match this target is left exactly as it
     was found -- untouched in the registry, not discarded -- because a
     mismatch here proves nothing about whether it might still be a valid
     target for a LATER kill of its own holder;
  2. re-verifies each remaining (generation-matched) candidate is still a
     live process-group leader (and, on Linux, that its process start time
     still matches what was recorded) immediately before signalling it;
     only then
  3. killpg's what is left.

The report tells you which of four things happened, because they must
not be read as the same outcome:

  - "swept N registered ... group(s)": reached and killed.
  - "... registered ... under a generation different from the one just
    confirmed dead ... left untouched": a registration existed but does
    not belong to the holder just confirmed dead (a different owner
    registered it, before or after) -- the child tree it points at may
    still be running, and it remains in the registry for a future kill of
    its own holder to act on.
  - "... no longer look like the process that registered them ... NOT
    reached": the generation matched but the process group itself no
    longer looks plausible (already exited, or the number was recycled).
  - "... could not be confirmed killed or already dead this attempt ...
    kept in the registry for a retry": the generation matched and the
    process group still looked plausible, but the kill signal itself
    returned neither success nor "already gone" (e.g. a permission
    error) -- run this command again to retry.

If none of those lines appear at all, no registration was ever found for
this session, which most commonly means the CLI-provider child either
never started or was never tracked. In every case above except the first,
you would need to find and kill the child tree separately (e.g.
` + "`pkill -f claude`" + `) or retry this command. This whole paragraph does
not apply on Windows, where ` + "`taskkill /F /T`" + ` plus the Job Object
tracking described above already reaches the full tree unconditionally.

The lock FILE is only removed once the holder is PROVEN dead (either the
OS lock was acquirable — nobody holds it — or the kill was issued and the
PID was observed to exit) AND a fresh OS-lock probe still succeeds right
before the unlink. If the kill fails, the PID is still alive after the
wait, or the lock state is unknown, the file is LEFT IN PLACE — an empty
lock file with no held OS lock is harmless (the next acquirer simply
reopens and overwrites it; see internal/session/lock.go), and removing a
path out from under a live OS lock is exactly the two-owners bug this
command must never trigger. Pass --keep-lock to skip the file removal
entirely even on a confirmed kill.`,
	Example: `
rush sessions kill pr-42
rush sessions kill pr-42 --keep-lock     # just kill, leave the lock file
rush sessions kill pr-42 --wait 10s      # wait up to 10s for the PID to die
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsKillCmdRun,
}

func sessionsKillCmdRun(cmd *cobra.Command, args []string) error {
	id := args[0]
	keepLock, _ := cmd.Flags().GetBool("keep-lock")
	wait, _ := cmd.Flags().GetDuration("wait")
	if wait <= 0 {
		wait = 5 * time.Second
	}

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}

	// Resolve the data directory the same lightweight way `stats` does:
	// honor --data-dir first, then the project's configured
	// data_directory, and only fall back to <cwd>/.rush if neither is
	// set. config.ResolveDataDirectory is pure, local, filesystem-only
	// config resolution (no network fetch, no DB connection, no config
	// persistence side effects), so it stays safe to use from a rescue
	// command that must keep working even when the network/DB/app is
	// stuck.
	//
	// Always use the resolved value, never the raw --data-dir flag
	// string directly: a RELATIVE --data-dir must be resolved against
	// --cwd (like setupApp/sessions.go's reset --force does), not
	// against the process's actual cwd, which can differ. See task #224
	// finding 1.
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
	if err != nil {
		return fmt.Errorf("failed to resolve data directory: %w", err)
	}

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(id)+".lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no lock file at %s\n", lockPath)
			return nil
		}
		return fmt.Errorf("stat lock: %w", err)
	}

	pid := session.ReadLockPID(lockPath)
	kr := probeThenKillHolder(dataDir, id, pid, wait)
	fmt.Fprint(os.Stderr, kr.Report)

	// Sweep registered CLI-provider child process groups ONLY once the old
	// holder is CONFIRMED dead (never for holderStillAlive/holderUnidentified/
	// holderProbeError -- a live or unidentified holder own child-group
	// entries are still live under kr.VictimGeneration, and sweeping them
	// while the holder itself might still be running would signal a process
	// group the holder is actively using). Uses sweepChildGroupsUnderOwnLock,
	// which acquires the session OS lock itself (the probe above already
	// released its own) and holds it across the whole read/verify/kill/
	// rewrite -- see that function doc comment for the full P0-2 rationale.
	// kr.VictimGeneration is empty whenever no generation sidecar existed to
	// read at all (a lock file predating the generation mechanism, or one
	// that never had a holder), which the sweep helper itself also treats
	// as a no-op -- this condition just avoids the acquire attempt entirely
	// in that case. As of task #602, this also covers the holderAlreadyDead
	// state (the lock was already free when probed, e.g. the previous
	// holder crashed instead of being killed by this command): see
	// probeThenKillHolder's own comment for where that generation is
	// captured and why it must be read before this command's own probe
	// acquire, not after.
	if kr.ConfirmedDead && kr.VictimGeneration != "" {
		var sweepReport strings.Builder
		sweepChildGroupsUnderOwnLock(&sweepReport, dataDir, id, kr.VictimGeneration)
		fmt.Fprint(os.Stderr, sweepReport.String())
	}

	if keepLock {
		fmt.Fprintf(os.Stderr, "lock file kept at %s (age %ds)\n", lockPath, age(info))
		return nil
	}

	// The lock file is removed ONLY once the holder is proven dead. Removing
	// it while a holder may still be alive (kill failed, the PID survived the
	// wait, or the probe was inconclusive) risks unlinking a live OS lock's
	// path — on POSIX flock is bound to the inode so the unlink doesn't
	// revoke the live holder, it just lets a second process create a fresh
	// inode at the same path and believe it owns the session: two owners of
	// one session id. An empty lock file left behind is harmless (the next
	// acquirer reopens and overwrites it), so fail closed here.
	if !kr.ConfirmedDead {
		fmt.Fprintf(os.Stderr, "warning: holder not confirmed dead; leaving lock file %s in place\n", lockPath)
		return fmt.Errorf("could not confirm the lock holder is dead; lock file left in place at %s", lockPath)
	}

	// If the probe above took the "already dead" branch (probeThenKillHolder's
	// own TryAcquireSessionLock+Release), that Release() returns as soon as
	// its synchronous unlock/close finish (session.SessionLock's Mechanism-1
	// fix) — its background metadata-cleanup goroutine continues running
	// without the OS lock. The generation-aware clearHolderMetadata
	// will skip destructive operations if a new owner has already acquired
	// the lock, so this wait is not strictly required for correctness,
	// but it avoids racing the background goroutine's file operations.
	// Without waiting, the SECOND probe immediately below could spuriously
	// observe SessionLockBusyError against our OWN prior release's
	// background cleanup goroutine. This is a bounded wait on a background
	// goroutine finishing syscalls that normally take microseconds — not a new
	// unbounded block.
	waitForLockMetadataSettled(lockPath, wait)

	// Final OS-level proof immediately before the destructive remove: a NEW
	// owner may have grabbed the session between the kill above and now. If
	// the real OS lock is currently held (or its state is unknown), leave the
	// file rather than risk unlinking a live holder's path.
	if !lockHolderProvablyDead(dataDir, id) {
		fmt.Fprintf(os.Stderr, "warning: lock is currently held (or its state is unknown); leaving lock file %s in place\n", lockPath)
		return nil
	}
	if err := removeLockWithRetry(lockPath, wait); err != nil {
		return fmt.Errorf("remove lock %s: %w (the process may still hold the handle — retry in a moment)", lockPath, err)
	}
	fmt.Fprintf(os.Stderr, "removed lock %s\n", lockPath)
	return nil
}

// waitForLockMetadataSettled polls lockPath's recorded PID until it reads 0
// (cleared) or budget elapses. Used between two back-to-back probes
// (TryAcquireSessionLock+Release cycles) against the same lock path: a probe's
// own Release() returns as soon as its synchronous unlock/close finish (see
// session.SessionLock.Release's Mechanism-1 doc comment), while its
// background metadata-cleanup goroutine continues running without the OS lock.
// A second probe launched immediately can otherwise race that goroutine and
// observe SessionLockBusyError against the FIRST probe's own in-flight cleanup,
// mistaking it for a genuine new holder. Bounded by budget (capped at 2s,
// since cleanup normally finishes in microseconds — this only ever waits
// meaningfully under genuine filesystem contention, which the caller's own
// lockHolderProvablyDead re-check handles safely either way). Best-effort:
// never returns an error, since a caller-visible timeout here would be worse
// than falling through to the caller's own authoritative probe.
//
// Note: As of the generation-aware clearHolderMetadata fix, this function
// is no longer strictly required for correctness — clearHolderMetadata
// will skip destructive operations if a new owner has already acquired the
// lock via its generation check — but it remains useful to avoid the narrow
// window where the background goroutine is still mid-cleanup and the second
// probe could spuriously observe contention against the first release's
// own cleanup goroutine.
func waitForLockMetadataSettled(lockPath string, budget time.Duration) {
	if budget <= 0 || budget > 2*time.Second {
		budget = 2 * time.Second
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if session.ReadLockPID(lockPath) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holderState constants describe what probeThenKillHolder concluded and did.
// Only the decision-relevant distinction matters to callers: ConfirmedDead
// (true only when the holder is proven gone at the OS level) gates every
// destructive follow-up. The State label is for operator-facing reports.
const (
	// holderAlreadyDead: the probe itself acquired the OS lock — nobody holds
	// it — so nothing was killed.
	holderAlreadyDead = "already-dead"
	// holderKilled: the probe found live contention and forceKillHolder
	// observed the PID exit within the wait budget.
	holderKilled = "killed"
	// holderStillAlive: a kill was issued but the PID was still alive when the
	// wait budget elapsed. NOT confirmed dead.
	holderStillAlive = "still-alive"
	// holderProbeError: TryAcquireSessionLock returned a non-busy error
	// (permission denied, IO error, ...). The lock state is genuinely unknown,
	// so nothing was killed (fail closed).
	holderProbeError = "probe-error"
	// holderUnidentified: the probe proved a live holder exists (busy error)
	// but neither the sidecar nor the lock file yielded a usable PID, so there
	// is nothing to kill and nothing can be confirmed dead. Distinct from
	// holderAlreadyDead precisely because a holder demonstrably DOES exist.
	holderUnidentified = "unidentified-holder"
)

// killResult is the structured outcome of probeThenKillHolder. ConfirmedDead
// is true ONLY when the holder has been proven gone at the OS level — either
// the probe acquired the lock (nobody holds it) or forceKillHolder observed
// the recorded PID exit. Callers MUST NOT perform any destructive follow-up
// (removing the lock file, wiping a session) unless ConfirmedDead is true; a
// false here means the holder might still be alive and must be left alone.
type killResult struct {
	State         string
	KilledPID     int
	ConfirmedDead bool
	Report        string
	// VictimGeneration is the session lock generation token belonging to
	// whichever holder ConfirmedDead is being reported for — never re-read
	// after the fact. Populated two different ways depending on which
	// ConfirmedDead branch of probeThenKillHolder ran:
	//
	//   - holderKilled: read via session.ReadLockGeneration in the SAME
	//     branch that decided to kill KilledPID, while busyErr still proved
	//     the OLD holder owned the lock (the P0-2 fix, 2026-08-19
	//     static-follow-up review) — this is the immutable identity token
	//     that fix requires: the child-group sweep must act ONLY on entries
	//     carrying THIS exact generation, never "whatever generation
	//     happens to be on disk when the sweep runs".
	//   - holderAlreadyDead: read BEFORE probeThenKillHolder's own
	//     TryAcquireSessionLock call, which (on success) immediately
	//     overwrites the ".gen" sidecar with its OWN new token — this is
	//     the orphaned holder's generation, the only point at which it is
	//     still readable at all (task #602, follow-up to #594: a holder
	//     that crashed instead of being killed by this command never runs
	//     Release(), so nothing else ever touches or clears its sidecar
	//     until the NEXT acquirer, including this probe's own, overwrites
	//     it).
	//
	// Empty when no generation sidecar existed to read (a lock file
	// predating the generation mechanism, one that never had a holder, or
	// the probe-error/unidentified-holder states, which are not
	// ConfirmedDead at all). See sweepChildGroupsUnderOwnLock /
	// sweepChildGroupsWithHeldLock below and
	// session.KillRegisteredChildGroups' doc comment for the full defect
	// the holderKilled case closes, and probeThenKillHolder's
	// holderAlreadyDead branch for the orphaned-generation case. A caller
	// that got ConfirmedDead=true with a non-empty VictimGeneration is the
	// only one authorized to sweep; callers must carry this value down to
	// the sweep call themselves rather than recomputing it, since
	// recomputing it is exactly the bug both fixes closed.
	VictimGeneration string
}

// probeThenKillHolder decides, via a real OS-level lock attempt, whether the
// PID recorded in the lock file is still a genuine live holder before handing
// off to forceKillHolder.
//
// Why: the lock file's PID is metadata, not proof. A holder that exited
// cleanly runs Release(), which clears that metadata (see
// session.SessionLock.Release), but an OLD lock file predating that fix, or
// one whose Release() never ran (process crashed and the file was left
// mid-write, or something wrote the file directly, as tests sometimes do)
// can still carry a stale PID. If the OS has since recycled that PID number
// for a totally unrelated process — routine on a busy CI/dev box — blindly
// kill(pid)-ing it would take out an innocent process.
//
// The only authoritative signal for "is anyone actually still holding this
// lock" is attempting to take the real OS lock ourselves:
//   - If TryAcquireSessionLock SUCCEEDS, nobody holds the lock right now,
//     full stop (see the package doc on SessionLock) — the recorded PID, if
//     any, is stale/dead/reused. We must NOT touch that PID. Release our
//     probe lock immediately and report ConfirmedDead (the holder is gone).
//   - If it reports *session.SessionLockBusyError, someone genuinely holds
//     the OS lock right now — the classic case this command exists for —
//     proceed to forceKillHolder exactly as before.
//   - Any OTHER error (permission denied, IO error, etc.) is NOT proof of
//     either state. FAIL CLOSED: return holderProbeError with
//     ConfirmedDead=false and do NOT kill the recorded PID. The previous
//     behavior fell back to unconditionally killing whatever PID was
//     recorded on any unidentified probe failure — exactly the stale/reused
//     PID danger the busy/acquire branches exist to avoid, just reached via a
//     different path. An unrelated IO hiccup must not turn this command into
//     "kill an arbitrary PID number off disk".
//
// What this does NOT solve: a residual PID-reuse TOCTOU window between "probe
// proved contention" and "the PID is actually killed". The busy-error branch
// proves someone holds the lock at the moment TryAcquireSessionLock ran, but
// forceKillHolder then makes its own separate IsProcessAlive + KillProcess
// syscalls at a later moment. In principle the OS could recycle that exact PID
// for an unrelated process in the gap. Closing this fully would require
// retaining an OS-level handle to the process at probe time and killing
// through that handle (Windows HANDLE could; POSIX needs pidfd). Given this is
// a manual rescue tool an operator runs deliberately, that structural fix is
// not implemented; the window is accepted as a narrow, known limitation.
func probeThenKillHolder(dataDir, sessionID string, pid int, wait time.Duration) killResult {
	// Read whatever generation token is CURRENTLY on disk before attempting
	// the acquire below (task #602, follow-up to the P0-2 fix/#594). This
	// is deliberately read BEFORE, not after, TryAcquireSessionLock: a
	// successful acquire immediately overwrites the ".gen" sidecar with a
	// brand-new token of its own (acquireSessionLockFileWithOptions writes
	// it synchronously, before TryAcquireSessionLock ever returns to this
	// function) and, once this probe releases below, the background
	// clearHolderMetadata cleanup goroutine deletes that sidecar entirely.
	// So this is the ONLY point at which a crashed holder's own generation
	// -- untouched since the crash, because a crash means its own Release()
	// never ran to overwrite or clear it -- can still be read at all.
	//
	// If the lock turns out to be BUSY (a live holder), this value is
	// simply discarded below in favor of the one read inside the busy
	// branch itself, which is the one actually proven to belong to the
	// live holder being killed. If the lock turns out to be FREE (the
	// branch immediately below), this value is the orphaned generation
	// left behind by whatever process held it last and never released
	// cleanly -- see the ConfirmedDead branch below for why sweeping that
	// generation now is what task #602 fixes.
	orphanedGeneration := session.ReadLockGeneration(session.SessionLockPath(dataDir, sessionID))

	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		// Proved nobody holds the OS lock. Whatever PID is recorded is stale.
		_ = lk.Release()
		var sb strings.Builder
		if pid > 0 {
			fmt.Fprintf(&sb, "lock probe acquired the lock: PID %d is stale (holder already gone); not killing anything\n", pid)
		} else {
			sb.WriteString("lock probe acquired the lock: no live holder; nothing to kill\n")
		}
		// task #602: the #594 fix made KillRegisteredChildGroups fence
		// strictly on an immutable victimGeneration the caller proves
		// belongs to a genuine holder -- correct, but it left the "the
		// holder crashed instead of being killed by us" case with no
		// victimGeneration at all, since this branch (the OS lock was
		// already free when probed) never goes through the busy branch
		// below that captures one. Without a victimGeneration here,
		// sessionsKillCmdRun's sweep call is skipped entirely (see its own
		// "kr.VictimGeneration != ''" gate), so a process-group tree
		// orphaned by a crash was never reachable by "sessions kill" at
		// all, and its registry entry could never be dropped either (the
		// #594 fix retains, rather than discards, any entry whose
		// generation does not match the sweep's target -- correct in
		// general, but with no sweep ever running for this generation, the
		// entry was retained forever).
		//
		// orphanedGeneration (read above, before this function's own
		// acquire overwrote the sidecar) is exactly the identity this
		// case needs: it is the generation of whichever process held the
		// lock immediately before this probe found it free, captured
		// while it was still the only thing on disk claiming that lock
		// generation. It is empty when there was never a generation
		// sidecar to read (a lock file predating the generation
		// mechanism, or one that never had a holder at all) -- callers and
		// KillRegisteredChildGroups itself both already treat an empty
		// victimGeneration as "sweep nothing", so this stays safe by
		// construction in that case.
		//
		// This does NOT reopen the SIGKILL-unverified-PID hazard the
		// registry's own review history warns about: the sweep this value
		// feeds into (KillRegisteredChildGroups) never signals a pgid on
		// generation match alone -- every matching entry is independently
		// re-verified via verifyGroupStillPlausibleOutcome (pgid group
		// leadership plus, on Linux, start-time match) immediately before
		// any kill(2) call. A generation match only selects WHICH entries
		// are even considered; it never substitutes for the plausibility
		// check.
		return killResult{State: holderAlreadyDead, ConfirmedDead: true, Report: sb.String(), VictimGeneration: orphanedGeneration}
	}
	var busyErr *session.SessionLockBusyError
	if errors.As(err, &busyErr) {
		// A real process holds the OS lock right now -- this IS the moment of
		// proven contention. Capture the victim's generation token HERE,
		// before forceKillHolder signals anything: this is the immutable
		// identity (holder PID, victim generation) pair the P0-2 fix
		// requires (see killResult.VictimGeneration's doc comment). On Unix,
		// killing the holder below releases the OS lock, and a brand-new
		// owner can acquire it and register its own child group under a NEW
		// generation before the sweep ever runs -- reading the generation
		// again at that later point would silently switch targets to the
		// new owner. Reading it now, while busyErr proves the OLD holder
		// still owns the lock, is the only point where this value is
		// trustworthy.
		victimGeneration := session.ReadLockGeneration(session.SessionLockPath(dataDir, sessionID))
		// Prefer the PID the probe itself identified (it reads the
		// never-locked sidecar) but fall back to the caller-supplied one for
		// safety.
		livePID := busyErr.HolderPID
		if livePID <= 0 {
			livePID = pid
		}
		kr := forceKillHolder(dataDir, sessionID, livePID, wait)
		kr.VictimGeneration = victimGeneration
		return kr
	}
	// Unidentified probe failure — NOT proof of either state. Fail closed:
	// never kill the recorded PID on an uncertain lock state.
	return killResult{
		State:  holderProbeError,
		Report: fmt.Sprintf("lock probe failed with an unidentified error (not proof of a live holder): %v\nrefusing to kill recorded PID %d — the lock state is unknown\n", err, pid),
	}
}

// forceKillHolder kills the PID (no-op for pid<=0) and waits up to `wait` for
// it to actually exit. Returns a structured killResult; ConfirmedDead is true
// only when the PID is observed to be gone. Safe to call when the process is
// already dead.
//
// Deliberately does NOT sweep child groups itself (an earlier version did,
// inline, immediately after observing the PID exit). The child-group sweep
// requires re-acquiring or already holding the session's OS lock across the
// whole read/verify/kill/rewrite (see session.KillRegisteredChildGroups'
// doc comment) -- a concern forceKillHolder's two callers resolve
// completely differently:
//   - probeThenKillHolder's caller (sessionsKillCmdRun) has released its own
//     probe lock and must explicitly re-acquire a NEW one for the sweep
//     (sweepChildGroupsUnderOwnLock).
//   - acquireSessionLockForReset calls this, then goes on to acquire the
//     lock itself and HOLD it across the caller's DB-reset critical
//     section -- the sweep there must run using THAT already-held lock
//     (sweepChildGroupsWithHeldLock), strictly after the re-acquire
//     succeeds, never before (see acquireSessionLockForReset's doc comment
//     for why "before" was the P0-2 defect for reset --force specifically).
//
// Interleaving either concern into forceKillHolder itself would make one of
// the two callers wrong by construction. See killResult.VictimGeneration
// for the generation token both callers must carry down to whichever sweep
// helper they use.
func forceKillHolder(dataDir, sessionID string, pid int, wait time.Duration) killResult {
	var sb strings.Builder
	if pid <= 0 {
		// This function is ONLY ever reached from the busy branch of
		// probeThenKillHolder / acquireSessionLockForReset — i.e. after the OS
		// has just proven that a live holder exists. So "no usable PID" here
		// does NOT mean "nobody is holding the lock"; it means a holder
		// demonstrably exists and we cannot identify it. Reporting
		// ConfirmedDead here would be a claim the code cannot support, and
		// every destructive follow-up is gated on exactly that field.
		sb.WriteString("a live holder holds the OS lock but no usable PID could be read " +
			"(empty/garbage lock file and no sidecar); nothing can be killed and nothing is confirmed dead\n")
		return killResult{State: holderUnidentified, ConfirmedDead: false, Report: sb.String()}
	}
	if !session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "PID %d already gone\n", pid)
		return killResult{State: holderAlreadyDead, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
	}
	if err := session.KillProcess(pid); err != nil {
		// KillProcess itself errored. The PID may yet exit asynchronously
		// (kill is async on Windows), so still poll the wait budget — but if
		// it is alive at the end, ConfirmedDead stays false.
		fmt.Fprintf(&sb, "kill PID %d: %v\n", pid, err)
	} else {
		fmt.Fprintf(&sb, "killed PID %d\n", pid)
	}
	// Poll until dead or wait elapses (taskkill/SIGKILL is async).
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !session.IsProcessAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if session.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "warning: PID %d still alive after %s wait\n", pid, wait)
		return killResult{State: holderStillAlive, KilledPID: pid, ConfirmedDead: false, Report: sb.String()}
	}
	fmt.Fprintf(&sb, "PID %d exited\n", pid)
	return killResult{State: holderKilled, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
}

// formatChildGroupSweepReport renders a session.ChildGroupSweepResult the
// same way regardless of which sweep helper produced it, so an operator
// sees identical wording from "sessions kill" and "sessions reset --force".
// Shared by sweepChildGroupsUnderOwnLock and sweepChildGroupsWithHeldLock.
func formatChildGroupSweepReport(sb *strings.Builder, sessionID string, result session.ChildGroupSweepResult) {
	if result.Killed > 0 {
		fmt.Fprintf(sb, "swept %d registered CLI-provider child process group(s) for session %s\n", result.Killed, sessionID)
	}
	if result.GenerationMismatch {
		fmt.Fprintf(sb, "a CLI-provider child process group was registered for session %s under a generation different from the one just confirmed dead; left untouched in the registry (it may belong to a live new owner) -- run sessions kill again once that owner is also done, if it still needs cleanup\n", sessionID)
	}
	if result.Implausible > 0 {
		fmt.Fprintf(sb, "%d registered CLI-provider child process group(s) for session %s no longer look like the process that registered them; NOT reached -- check for it manually\n", result.Implausible, sessionID)
	}
	if result.Retained > 0 {
		fmt.Fprintf(sb, "%d registered CLI-provider child process group(s) for session %s could not be confirmed killed or already dead this attempt; kept in the registry for a retry -- run sessions kill again\n", result.Retained, sessionID)
	}
}

// sweepChildGroupsUnderOwnLock is the "sessions kill" half of the P0-2 fix
// (2026-08-19 static-follow-up review). Called after probeThenKillHolder has
// confirmed the OLD holder is dead and released its own probe lock (Release()
// on POSIX drops the OS lock the instant the holder process exits -- this
// function is NOT the first thing to touch the lock after that). It must
// therefore prove for itself, right now, that no new owner already holds the
// session before it is safe to read/kill/rewrite the child-group registry:
//
//  1. Acquire the session's OWN OS lock (session.TryAcquireSessionLock). If
//     that FAILS with contention, a new owner already holds the session --
//     do NOT sweep at all. Their own live child-group registration (if any)
//     must not be read, let alone rewritten, without holding the same lock
//     they are relying on for exclusion; and per victimGeneration fencing
//     (see session.KillRegisteredChildGroups doc comment), the dead holder's
//     own entries are safe to leave for a LATER sweep in any case -- they do
//     not become less identifiable while a new owner runs.
//  2. Hold that lock across the ENTIRE session.KillRegisteredChildGroups
//     call -- read, verify, kill, and the write-back at the end -- so a new
//     RegisterChildGroup cannot land in the middle and be lost to a stale
//     retained-snapshot rewrite (the cross-process lost-update race the same
//     review documented).
//  3. Release the lock immediately after, so a legitimate new owner is not
//     kept waiting for this rescue tool cleanup pass.
//
// victimGeneration is the immutable token probeThenKillHolder captured at
// the moment it proved the OLD holder was live (killResult.VictimGeneration)
// -- session.KillRegisteredChildGroups signals ONLY entries carrying this
// exact generation, never whatever generation a fresh read of the lock file
// would report now.
func sweepChildGroupsUnderOwnLock(sb *strings.Builder, dataDir, sessionID, victimGeneration string) {
	if victimGeneration == "" {
		// Nothing to fence against -- e.g. an old, pre-generation lock file,
		// or a caller that never proved contention. Sweeping with an empty
		// target would (correctly, per KillRegisteredChildGroups own empty
		// handling) touch nothing, but skip the acquire/release cycle
		// entirely rather than pretend there is a sweep to report.
		return
	}
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		// Either a new owner already holds the session (SessionLockBusyError)
		// or the probe itself is unidentified -- either way, NOT safe to
		// touch the registry: see this function doc comment, point 1.
		fmt.Fprintf(sb, "could not acquire the session lock to sweep CLI-provider child process groups for session %s (a new owner may already hold it): %v -- left untouched, retry sessions kill once it is free\n", sessionID, err)
		return
	}
	defer lk.Release()
	result := session.KillRegisteredChildGroups(dataDir, sessionID, victimGeneration)
	formatChildGroupSweepReport(sb, sessionID, result)
}

// sweepChildGroupsWithHeldLock is the "sessions reset --force" half of the
// P0-2 fix. Unlike sweepChildGroupsUnderOwnLock, the caller
// (acquireSessionLockForReset) is already holding the session OS lock by
// the time this runs -- either because it just re-acquired it after killing
// a live holder, or because its very first TryAcquireSessionLock attempt
// succeeded outright (no live holder at all, e.g. the previous holder had
// already crashed; see acquireSessionLockForReset's task #602 comment) --
// that lock is what "sessions reset --force" needs held across its own
// DB-wipe critical section anyway (see acquireSessionLockForReset doc
// comment), so this function does not acquire or release anything itself;
// it just performs the sweep using the lock the caller already has, which
// is exactly the same "hold the OS lock across the entire
// read/verify/kill/rewrite" requirement session.KillRegisteredChildGroups
// documents. Calling this BEFORE the (re-)acquire (as an earlier version of
// "reset --force" did, via forceKillHolder inline sweep) is precisely the
// ordering half of P0-2: it guaranteed the sweep ran in the exact window
// where a new owner could already be in, with no lock held by anyone doing
// the sweeping.
//
// lk is accepted (and required non-nil) purely to make the "the caller
// must already hold the lock" precondition visible at every call site --
// it is not otherwise used, since session.KillRegisteredChildGroups takes
// dataDir/sessionID directly rather than an open lock handle.
func sweepChildGroupsWithHeldLock(sb *strings.Builder, dataDir, sessionID, victimGeneration string, lk *session.SessionLock) {
	if victimGeneration == "" || lk == nil {
		return
	}
	result := session.KillRegisteredChildGroups(dataDir, sessionID, victimGeneration)
	formatChildGroupSweepReport(sb, sessionID, result)
}

// acquireSessionLockForReset acquires the real OS session lock, killing the
// current holder first if one is alive (and confirmable). It returns the HELD
// lock (caller MUST Release) so no concurrent rush process can grab the
// session during a critical section that follows — in particular `sessions
// reset --force`'s DB wipe, which must not race a fresh `rush run --session
// <id>` that recreates the lock at the same path and starts writing.
//
// This is the only place in the sessions CLI that holds the OS lock across a
// non-removal operation, and it is correct on both platforms precisely BECAUSE
// it does not unlink the path while holding it: holding a lock to serialize
// access is exactly what OS locks are for. (Unlinking while holding is the
// Windows-fragile case the lockHolderProvablyDead doc comment rejects; this
// path avoids it entirely by never removing the file.)
//
// Fails closed — returns a non-nil error and a nil lock whenever the holder
// could not be proven dead (kill failed, PID still alive, or the probe was
// inconclusive). It NEVER falls back to "act without the lock": a reset that
// proceeds without holding the lock would race a live/new holder against the
// DB wipe, which is strictly worse than refusing to reset.
//
// P0-2 fix (2026-08-19 static-follow-up review): this function also now
// sweeps registered CLI-provider child process groups for the killed
// holder, but ONLY after the re-acquire above has succeeded and using the
// lock JUST re-acquired (see sweepChildGroupsWithHeldLock, called near the
// end of the body below). An earlier version ran the equivalent sweep
// (then inline inside forceKillHolder) immediately after confirming the
// old holder dead but BEFORE this function re-acquired the lock -- exactly
// the window where a brand-new owner could already have taken the session
// and registered its own live child group, with no lock held by anyone
// doing the sweeping. Moving the sweep to strictly after a successful
// re-acquire, combined with fencing on the victim generation captured
// before the kill (not whatever generation happens to be on disk when the
// sweep runs), closes both the wrong-target-kill and the lost-durable-
// pointer halves of that defect for the reset --force path specifically.
//
// task #602 follow-up: the branch immediately below (TryAcquireSessionLock
// succeeds on the FIRST attempt -- nobody held the lock at all, e.g. the
// previous holder crashed instead of being killed by this command) used to
// return with an empty VictimGeneration, exactly the same gap probeThenKillHolder
// had for `sessions kill`'s equivalent branch -- see that function's own
// #602 comment for the full mechanism (a successful acquire immediately
// overwrites the ".gen" sidecar with THIS acquire's own new token, so the
// crashed holder's generation must be read BEFORE this call, never after).
// Reading it here is actually cheaper than sessions kill's case: the lock
// this function acquires is already held and returned to the caller, which
// already holds it across the DB-wipe critical section that follows -- so
// the sweep below reuses sweepChildGroupsWithHeldLock (the same helper the
// busy/killed branch already uses), no second acquire/release cycle needed.
func acquireSessionLockForReset(dataDir, sessionID string, pid int, wait time.Duration) (*session.SessionLock, killResult, error) {
	// Read BEFORE the acquire attempt below, for the same reason
	// probeThenKillHolder does: a successful TryAcquireSessionLock
	// immediately overwrites the ".gen" sidecar with this call's own new
	// generation, so any process that held the lock before this moment --
	// including one that crashed and never got to run Release() -- has its
	// generation token read here or not at all.
	orphanedGeneration := session.ReadLockGeneration(session.SessionLockPath(dataDir, sessionID))
	lk, err := session.TryAcquireSessionLock(dataDir, sessionID)
	if err == nil {
		kr := killResult{State: holderAlreadyDead, ConfirmedDead: true, Report: "lock acquired; no live holder\n", VictimGeneration: orphanedGeneration}
		// Sweep now, using the lock just acquired and about to be returned
		// HELD to the caller -- exactly the same "hold across the whole
		// read/verify/kill/rewrite" requirement session.KillRegisteredChildGroups
		// documents, and the same helper (sweepChildGroupsWithHeldLock) the
		// busy/killed branch below uses after its own re-acquire.
		var sweepReport strings.Builder
		sweepChildGroupsWithHeldLock(&sweepReport, dataDir, sessionID, orphanedGeneration, lk)
		kr.Report += sweepReport.String()
		return lk, kr, nil
	}
	var busyErr *session.SessionLockBusyError
	if !errors.As(err, &busyErr) {
		// Unknown probe failure — fail closed; never act without the lock.
		return nil, killResult{State: holderProbeError, Report: fmt.Sprintf("lock probe failed (state unknown): %v\n", err)},
			fmt.Errorf("could not determine lock state for session %s: %w", sessionID, err)
	}
	// A live process holds the lock right now -- this IS the moment of
	// proven contention. Capture the victim generation token HERE, exactly
	// like probeThenKillHolder does, before forceKillHolder signals
	// anything: see killResult.VictimGeneration doc comment and this
	// function own doc comment (2026-08-19 static-follow-up review, P0-2)
	// for why re-deriving this value later, after the kill, would silently
	// target whichever generation happens to be current by then instead of
	// the one actually being killed.
	victimGeneration := session.ReadLockGeneration(session.SessionLockPath(dataDir, sessionID))
	// A live process holds the lock. Kill it, wait for confirmed death, then
	// acquire so we hold it through the critical section that follows.
	livePID := busyErr.HolderPID
	if livePID <= 0 {
		livePID = pid
	}
	kr := forceKillHolder(dataDir, sessionID, livePID, wait)
	kr.VictimGeneration = victimGeneration
	if !kr.ConfirmedDead {
		return nil, kr, fmt.Errorf("could not confirm the lock holder (PID %d) is dead; session left untouched", livePID)
	}
	lk, err = session.TryAcquireSessionLock(dataDir, sessionID)
	if err != nil {
		if errors.As(err, &busyErr) {
			return nil, kr, fmt.Errorf("session %s is still locked after killing PID %d; another process may have acquired it", sessionID, livePID)
		}
		return nil, kr, fmt.Errorf("could not re-acquire lock for session %s after kill: %w", sessionID, err)
	}
	// Sweep child-process groups ONLY now, strictly AFTER the re-acquire
	// above succeeded, using the lock we JUST acquired and are about to
	// return HELD to the caller. This ordering is the actual reset --force
	// half of the P0-2 fix: an earlier version ran the equivalent sweep
	// (then inline inside forceKillHolder) BEFORE this re-acquire, which
	// guaranteed it could run in the exact window where a brand-new owner
	// had already taken the session and registered its own live child
	// group -- with no lock held by anyone doing the sweeping, since the
	// old holder lock died with it and this one had not been taken yet.
	// sweepChildGroupsWithHeldLock does not acquire or release lk itself
	// (the caller of THIS function holds it across the DB-wipe critical
	// section that follows), it just performs the sweep using it.
	var sweepReport strings.Builder
	sweepChildGroupsWithHeldLock(&sweepReport, dataDir, sessionID, victimGeneration, lk)
	kr.Report += sweepReport.String()
	return lk, kr, nil
}

// removeLockWithRetry tries to delete the lock file until it succeeds or
// `wait` elapses. On Windows the file handle held by a just-killed
// process can take a moment to release; an immediate Remove returns
// ERROR_SHARING_VIOLATION ("the process cannot access the file because
// it is being used by another process"). Retrying with a small backoff
// covers that window without a hardcoded sleep.
func removeLockWithRetry(path string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		err := os.Remove(path)
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return lastErr
}

func age(info os.FileInfo) int {
	if info == nil {
		return 0
	}
	return int(time.Since(info.ModTime()).Seconds())
}

// sanitiseSessionIDForFilename mirrors session.sanitiseSessionID (package-private)
// so the lock-file path resolves the same way the lock acquirer wrote it.
func sanitiseSessionIDForFilename(id string) string {
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		`"`, "_",
		"<", "_",
		">", "_",
		"|", "_",
		" ", "_",
	)
	return r.Replace(id)
}

func init() {
	sessionsKillCmd.Flags().Bool("keep-lock", false, "Kill the process but do not delete the lock file")
	sessionsKillCmd.Flags().Duration("wait", 5*time.Second, "How long to wait for the PID to die and the OS to release the lock handle")
}
