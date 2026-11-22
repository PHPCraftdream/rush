package sesslocks

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
)

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
	orphanedGeneration := sessionlock.ReadLockGeneration(sessionlock.SessionLockPath(dataDir, sessionID))

	lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
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
	var busyErr *sessionlock.SessionLockBusyError
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
		victimGeneration := sessionlock.ReadLockGeneration(sessionlock.SessionLockPath(dataDir, sessionID))
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
//   - AcquireSessionLockForReset calls this, then goes on to acquire the
//     lock itself and HOLD it across the caller's DB-reset critical
//     section -- the sweep there must run using THAT already-held lock
//     (sweepChildGroupsWithHeldLock), strictly after the re-acquire
//     succeeds, never before (see AcquireSessionLockForReset's doc comment
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
		// probeThenKillHolder / AcquireSessionLockForReset — i.e. after the OS
		// has just proven that a live holder exists. So "no usable PID" here
		// does NOT mean "nobody is holding the lock"; it means a holder
		// demonstrably exists and we cannot identify it. Reporting
		// ConfirmedDead here would be a claim the code cannot support, and
		// every destructive follow-up is gated on exactly that field.
		sb.WriteString("a live holder holds the OS lock but no usable PID could be read " +
			"(empty/garbage lock file and no sidecar); nothing can be killed and nothing is confirmed dead\n")
		return killResult{State: holderUnidentified, ConfirmedDead: false, Report: sb.String()}
	}
	if !procinfo.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "PID %d already gone\n", pid)
		return killResult{State: holderAlreadyDead, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
	}
	if err := procinfo.KillProcess(pid); err != nil {
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
		if !procinfo.IsProcessAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if procinfo.IsProcessAlive(pid) {
		fmt.Fprintf(&sb, "warning: PID %d still alive after %s wait\n", pid, wait)
		return killResult{State: holderStillAlive, KilledPID: pid, ConfirmedDead: false, Report: sb.String()}
	}
	fmt.Fprintf(&sb, "PID %d exited\n", pid)
	return killResult{State: holderKilled, KilledPID: pid, ConfirmedDead: true, Report: sb.String()}
}

// reacquireAfterKill retries the acquire while the lock reads busy, for up to
// wait. The OS releases a killed holder's flock/LockFileEx asynchronously
// (LockFileEx: the delay "depends upon available system resources"), so the
// first attempt right after confirmed death can still lose under load. A
// genuinely new owner just fails the same way, later.
func reacquireAfterKill(dataDir, sessionID string, wait time.Duration) (*sessionlock.SessionLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lk, err := sessionlock.TryAcquireSessionLock(dataDir, sessionID)
		var busyErr *sessionlock.SessionLockBusyError
		if err == nil || !errors.As(err, &busyErr) || !time.Now().Before(deadline) {
			return lk, err
		}
		time.Sleep(50 * time.Millisecond)
	}
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
