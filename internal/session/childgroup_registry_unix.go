//go:build !windows

// Cross-process child process-group registry (#580), generation-checked.
//
// Why this exists: "rush sessions kill <id>" runs as a SEPARATE OS process
// from the rush instance that holds the session lock. It reads the holder
// PID from the lock file/sidecar and kills that one PID via KillProcess,
// which on Unix also killpg's the PID's OWN process group if that PID
// happens to be a leader of ITS group. But a CLI provider child
// (claude/gemini/codex/qwen, see internal/agent/cliprovider) is
// deliberately spawned as the leader of its OWN, DIFFERENT process group
// (procgroup_unix.go's configureChildProcessGroup / go-pty's Setsid),
// precisely so a stray signal to rush's terminal/group does not also kill
// it (see track_unix.go's doc comment and commit 59908cbe). That same
// design choice means killing the rush PID alone -- all "sessions kill"
// can reach through the lock file -- does NOT touch the CLI child's group.
//
// KillAllTrackedTrees (in track_unix.go) solves this for the IN-PROCESS
// crash-net path because that code runs inside the same rush process that
// holds the in-memory trackedGroups map. "sessions kill" runs in a separate
// process and has no access to that map, so the fix needs a durable,
// cross-process record.
//
// SECURITY: an earlier version of this file keyed that record purely by
// the rush process's numeric pid, under os.TempDir() (world-writable on
// most POSIX systems, 0755/0644). That was rejected in review for two
// independent reasons and replaced with the design below:
//
//  1. PID reuse: a rush process that crashes never runs
//     UnregisterChildGroup/RemoveChildGroupRegistry, so the file survives
//     indefinitely. Once the OS recycles that pid for an unrelated
//     process, a LATER "sessions kill" against a session that happens to
//     reuse the same pid would read the dead instance's stale file and
//     SIGKILL whatever process groups now happen to own those pgids --
//     processes it was never pointed at.
//  2. World-writable plus predictable path: any local user can precompute
//     the tmp path for a pid they expect to see used and pre-seed it with
//     pgids of their choosing.
//
// This version fixes both:
//
//   - Keyed by (dataDir, sessionID), not bare pid, and stored NEXT TO the
//     session lock file it corresponds to (dataDir/locks/session-<id>.lock),
//     which is already per-user/per-project, not a shared world-writable
//     tmp directory.
//   - Every record carries the session lock's GENERATION TOKEN (see
//     internal/session/lock.go's writeGenerationSidecar and
//     ReadLockGeneration) captured at registration time.
//     KillRegisteredChildGroups refuses to signal anything unless the
//     recorded generation matches victimGeneration -- an IMMUTABLE token its
//     caller captured for the dead/killed holder BEFORE that holder's lock
//     was released, not whatever generation happens to be on disk at sweep
//     time (see commit 05a1708f / task #594: an earlier version compared
//     against a freshly re-read "current" on-disk generation, which raced a
//     new owner's own acquire-and-register between the victim's death and
//     the sweep) -- proving the lock has not been released and re-acquired
//     (by a restart, or a different session reusing the id after the first
//     died and was reaped) since registration. This is the exact mechanism
//     internal/session/lock.go's own clearHolderMetadata uses to guard its
//     own destructive cleanup; see ReadLockGeneration's doc for why a bare
//     pid is not an identity but a (session id, generation) pair is.
//   - Every pgid is re-validated for plausibility immediately before
//     signalling (see verifyGroupStillPlausible): still a signalable
//     registered group -- either the registered leader still leads it
//     (start-time-checked where the platform can), or the leader has been
//     reaped but the group still holds a living member whose registration-
//     time identity (pid + start time) is proven by a recorded witness.
//     This narrows, but as
//     documented on verifyGroupStillPlausible, does not fully close, the
//     residual pid-reuse race between validation and the signal itself.
//
// WRITE DURABILITY (2026-08-19 static-follow-up review, task #591 part 3):
// the registry file is written via a temp-file-in-same-dir + fsync + rename
// sequence (see writeChildGroupFileLocked), not a truncate-and-write
// os.WriteFile. An external kill (or crash) landing mid-write of a plain
// os.WriteFile could leave a later sweep reading an empty or partially
// written registry at exactly the moment the registry exists to be read --
// rename is atomic on POSIX filesystems (the readers this file has, all
// under childGroupFileMu or a fresh process's first read, only ever see the
// old complete file or the new complete file, never a half-written one).
//
// RETENTION ON SWEEP (same review, same task, part 3): KillRegisteredChildGroups
// no longer removes the registry file unconditionally after one attempt.
// Only entries that are CONFIRMED handled -- successfully killed, or
// confirmed already dead (ESRCH, or verifyGroupStillPlausibleOutcome
// reporting the pgid/start-time no longer matches what was registered) --
// are dropped. An entry left in an INCONCLUSIVE state (a kill syscall
// error that is neither "succeeded" nor "confirmed already gone", or a
// plausibility check that could not be completed) is written BACK to the
// registry, still fenced by the same (generation, start-time) pair, so a
// second "sessions kill" has a durable pointer to retry against instead of
// silently losing the only record of an unreached child tree.
//
// KNOWN LIMITATION (2026-10-19 review, P2-4): on platforms without a
// start-time/member-enumeration facility (macOS, BSD -- see
// starttime_other_unix.go) registration records NO witnesses, so a
// registered group whose leader has been reaped is RETAINED and NEVER
// signalled by the leader-gone branch of verifyGroupStillPlausibleOutcome:
// descendants of a vanished leader are NOT cleaned by the sweep there (they
// rely on their own parent-death handling). This is accepted conservative
// behaviour ("inconclusive, do not kill"), not a bug. Per-OS behaviour:
// Linux records and re-checks witnesses via /proc
// (starttime_linux.go's startTimeToken/groupMemberPIDs); macOS/BSD are
// inert in this branch (retained, never signalled).
package session

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// childGroupRegistryPath returns the sidecar file recording the CLI
// provider child process groups a session's rush process registered,
// alongside the lock generation token that was current when each entry was
// written. Lives next to the session lock file itself
// (dataDir/locks/session-<id>.lock), not in a shared world-writable
// directory: same permission model as the PID/generation sidecars already
// written there by internal/session/lock.go.
func childGroupRegistryPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".childgroups")
}

// childGroupFileMu serializes this process's own read-modify-write cycles
// against its own session's registry file. It does not, and CANNOT, protect
// against a concurrent writer for the SAME session in another process. An
// earlier version of this comment claimed "the session lock itself already
// guarantees only one process holds a given session at a time, so there is
// only ever one legitimate writer" -- that is false, and was the root cause
// of a real cross-process lost-update defect (2026-08-19 static-follow-up
// review, task #591 blocker P0-2): KillRegisteredChildGroups is a RESCUE
// TOOL that runs in a THIRD process, one that does not hold (and, in the
// buggy version, never even tried to acquire) the session's own OS lock
// while it read, verified, and wrote back the registry. That gap is exactly
// where a live new owner's RegisterChildGroup could land and be clobbered
// by the rescuer's stale retained-snapshot write. childGroupFileMu alone
// cannot close that: it is a process-local sync.Mutex and provides zero
// cross-process exclusion. The actual fix is at the call sites
// (sweepChildGroupsWithHeldLock / sweepChildGroupsUnderOwnLock in
// internal/cmd/sessions_kill.go), which now acquire (or already hold) the
// real session.SessionLock OS lock across the entire
// read/verify/kill/rewrite performed under this mutex, making the session
// lock -- not this mutex -- the thing that actually serializes writers
// across processes for the sweep's duration.
var childGroupFileMu sync.Mutex

// childGroupWitness is one member-identity record captured at registration
// time: a pid that was a non-leader member of the registered group then,
// and its start-time token at that moment (see startTimeToken). Witnesses
// are the ownership evidence that SURVIVES leader exit: at sweep time, in
// the leader-gone branch of verifyGroupStillPlausibleOutcome, the group may
// be signalled only if at least one witness pid still exists with the
// recorded start time (2026-10-18 review, finding 2 -- the old "identity by
// pid reservation" argument could not distinguish a surviving member of the
// registered group from members of an unrelated group a reused pgid number
// had since acquired).
type childGroupWitness struct {
	PID       int
	StartTime string
}

// childGroupEntry is one recorded process group: the pgid, and the session
// lock generation token that was live when it was registered.
type childGroupEntry struct {
	PGID       int
	Generation string
	// StartTime is a platform-specific process identity token captured at
	// registration time (Linux: /proc/pid/stat field 22, the process's
	// start time in clock ticks since boot -- monotonic per-pid until reuse,
	// see startTimeToken). Empty when the platform has no such facility
	// (macOS) or the capture failed. A non-empty value must still match at
	// signal time -- see verifyGroupStillPlausible.
	StartTime string
	// Witnesses are (pid, start-time) pairs of non-leader group members
	// captured at registration time, while the pgid was still a verified
	// live leader. Nil/empty when the platform cannot enumerate members
	// (macOS) or the group had no other members at registration. Entries
	// persisted by older versions carry none; for those the leader-gone
	// branch is inconclusive and the entry is retained, not signalled.
	Witnesses []childGroupWitness
}

// RegisterChildGroup durably records that the rush process currently
// holding sessionID's lock is responsible for tearing down the process
// group led by pgid if it is killed from outside without running its own
// cleanup. Call only for a confirmed process-group leader (pgid == its own
// pgid) -- see TrackProcessTree's identical constraint, which this is meant
// to be called alongside.
//
// generation MUST be the value session.ReadLockGeneration(lockPath) reports
// for THIS session at (or immediately before) registration time -- see
// ReadLockGeneration's doc for why a stale or empty generation makes this
// entry permanently unusable by design. KillRegisteredChildGroups does NOT
// compare this entry against whatever generation happens to be on disk at
// sweep time -- it requires an exact, non-empty match against
// victimGeneration, an IMMUTABLE token its caller captured at the moment it
// proved contention with the dead/killed holder, before ownership (and the
// on-disk generation) could change hands (see commit 05a1708f / task #594's
// fix and KillRegisteredChildGroups' own doc for why matching against a
// freshly re-read "current" generation was the bug this replaced).
//
// Best-effort: failures are logged, never returned as an error. A registry
// write failure only means sessions kill cannot reach the child tree; it
// does not change any other behavior.
func RegisterChildGroup(dataDir, sessionID string, pgid int, generation string) {
	if pgid <= 0 || generation == "" {
		return
	}
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	defer childGroupFileMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("session: failed to create child-group registry dir", "err", err)
		return
	}
	startTime, _ := startTimeToken(pgid)
	// Capture the member witness list NOW, while pgid is still a verified
	// live leader (the caller's contract): this is the only moment the
	// group's membership can be attributed to THIS registration. After the
	// leader exits, membership alone proves nothing -- any unrelated group
	// that later reuses the pgid number also has members (2026-10-18
	// review, finding 2). Capped so a pathological group cannot grow a
	// registry line without bound.
	witnesses := captureGroupWitnesses(pgid)
	existing := readChildGroupFileLocked(path)
	for i, e := range existing {
		if e.PGID == pgid && e.Generation == generation {
			// Re-registering the same (pgid, generation): refresh the
			// start-time token in case it was unavailable the first time
			// (e.g. captured before the child fully started) and is
			// available now. Same for the witness list, which may have
			// been empty because members had not been forked yet.
			refreshed := false
			if startTime != "" && e.StartTime == "" {
				existing[i].StartTime = startTime
				refreshed = true
			}
			if len(witnesses) > 0 && len(e.Witnesses) == 0 {
				existing[i].Witnesses = witnesses
				refreshed = true
			}
			if refreshed {
				writeChildGroupFileLocked(path, existing)
			}
			return
		}
	}
	existing = append(existing, childGroupEntry{PGID: pgid, Generation: generation, StartTime: startTime, Witnesses: witnesses})
	writeChildGroupFileLocked(path, existing)
}

// maxGroupWitnesses bounds how many member witnesses one registry entry
// records, keeping each registry line well under any sane line-length
// limit even for a group with hundreds of members.
const maxGroupWitnesses = 16

// captureGroupWitnesses records (pid, start-time) pairs for the current
// non-leader members of pgid, up to maxGroupWitnesses. Best-effort: members
// whose start-time token cannot be read (exited mid-capture, or a platform
// with no facility) yield no witness -- fewer witnesses only narrows what a
// later sweep may signal, never widens it.
func captureGroupWitnesses(pgid int) []childGroupWitness {
	var witnesses []childGroupWitness
	for _, pid := range groupMemberPIDs(pgid) {
		if len(witnesses) >= maxGroupWitnesses {
			break
		}
		start, ok := startTimeToken(pid)
		if !ok {
			continue
		}
		witnesses = append(witnesses, childGroupWitness{PID: pid, StartTime: start})
	}
	return witnesses
}

// UnregisterChildGroup removes pgid from sessionID's durable registry,
// mirroring UntrackProcessTree's in-memory forget. Safe to call for a pgid
// that was never registered. When the registry file becomes empty it is
// removed entirely so a long-lived rush process that starts and stops many
// CLI streams does not accumulate stale, empty sidecar files.
func UnregisterChildGroup(dataDir, sessionID string, pgid int) {
	if pgid <= 0 {
		return
	}
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	defer childGroupFileMu.Unlock()

	existing := readChildGroupFileLocked(path)
	filtered := existing[:0]
	for _, e := range existing {
		if e.PGID != pgid {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("session: failed to remove empty child-group registry file", "path", path, "err", err)
		}
		return
	}
	writeChildGroupFileLocked(path, filtered)
}

// readChildGroupFileLocked reads and parses the registry file for one
// session. Caller must hold childGroupFileMu. A missing or malformed file
// (or a malformed line within an otherwise-valid file) reads as nothing
// registered for that line rather than an error -- this registry is a
// best-effort augmentation, never a correctness dependency.
//
// Format, one entry per line:
//
//	pgid generation [leaderStart [pid@start ...]]
//
// The third field space-separated list starts with the leader's start-time
// token, followed by zero or more "pid@start" member-witness tokens (see
// childGroupWitness). 2-field lines (no third field) and 3-field lines
// carrying only the leader start time -- both written by older versions --
// parse with NO witnesses, which the sweep treats as "identity unverifiable
// in the leader-gone branch: retained, not signalled" (the conservative
// direction). Malformed witness tokens are skipped, also conservative.
func readChildGroupFileLocked(path string) []childGroupEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []childGroupEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: pgid, generation, optional starttime -- starttime is
		// omitted on platforms/captures where it is unavailable, so both
		// 2-field and 3-field lines parse.
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			continue
		}
		pgid, perr := strconv.Atoi(fields[0])
		if perr != nil || pgid <= 0 {
			continue
		}
		gen := strings.TrimSpace(fields[1])
		if gen == "" {
			continue
		}
		var startTime string
		var witnesses []childGroupWitness
		if len(fields) == 3 {
			// Field 3: leader start token, then optional "pid@start"
			// witness tokens -- see the format comment above.
			for i, tok := range strings.Fields(fields[2]) {
				if pidPart, start, hasAt := strings.Cut(tok, "@"); hasAt {
					wpid, werr := strconv.Atoi(pidPart)
					if werr != nil || wpid <= 0 || start == "" {
						continue
					}
					witnesses = append(witnesses, childGroupWitness{PID: wpid, StartTime: start})
					continue
				}
				if i == 0 {
					startTime = tok
				}
			}
		}
		entries = append(entries, childGroupEntry{PGID: pgid, Generation: gen, StartTime: startTime, Witnesses: witnesses})
	}
	return entries
}

// writeChildGroupFileLocked overwrites the registry file for one session.
// Caller must hold childGroupFileMu. Best-effort: a write failure is logged
// only.
//
// Atomic temp-file + fsync + rename + directory fsync (2026-08-19
// static-follow-up review, task #591 part 3; directory fsync added by the
// fourth review's P2-3), replacing a prior truncate-and-write os.WriteFile.
// The registry's whole purpose is to survive an external, out-of-process
// kill landing at an arbitrary moment -- an ordinary os.WriteFile truncates
// the file to zero length before writing the new bytes, so a kill (or
// crash) landing in that window leaves a later sweep reading an empty or
// partial file and concluding, wrongly, that nothing is registered. Writing
// to a temp file in the SAME directory (so the final rename is
// same-filesystem and therefore atomic on POSIX), fsync-ing it before the
// rename (so the rename cannot be reordered ahead of the data actually
// landing on disk by the OS's own write-back cache, which os.Rename's own
// atomicity guarantee does not by itself cover), and then os.Rename over
// the final path (an atomic directory-entry swap all POSIX filesystems this
// codebase targets support) means any reader -- including one racing this
// exact write -- only ever observes the complete OLD file or the complete
// NEW file, never a truncated or partially-written one.
//
// DURABILITY SCOPE: the sequence above is sufficient for the failure modes
// this registry actually exists to survive -- an external SIGKILL to the
// rush process, or that process crashing -- because the directory entry
// for `path` (pointing at the OLD inode, then the NEW one after rename)
// only needs to be correct for readers of the SAME still-mounted
// filesystem; a rename's directory-entry update is itself atomic content
// the kernel keeps consistent in its own page/dentry cache regardless of
// whether that cache has been flushed to the physical device yet. What the
// sequence above does NOT guarantee is durability across a power loss (or
// an unclean unmount) that happens before the kernel flushes the RENAME
// operation itself -- as opposed to the file's DATA, which the tmp.Sync()
// above already forces to disk -- to the underlying device: POSIX permits
// a directory's own metadata (including which inode a name currently
// points at) to be reordered independently of file data unless the
// directory itself is fsynced too. This function now does that: after a
// successful rename, it opens the parent directory and calls Sync on it,
// which is the standard "fsync the directory to persist a rename" idiom on
// Linux. fsync(2)'s NOTES put it plainly: calling fsync() on a file does
// not necessarily ensure that the entry in the directory containing it has
// also reached disk, and an explicit fsync() on a descriptor for the
// directory is needed for that. This closes the gap for filesystems/configurations
// where it matters; it remains a no-op in effect on filesystems where the
// rename was already durable without it. A directory-sync failure is
// logged only, exactly like every other step in this function -- the file
// rename itself already fully succeeded by the time this runs, and this is
// strictly an additional durability margin, not a correctness precondition
// for any reader (see the "complete OLD or complete NEW, never partial"
// guarantee above, which does not depend on the directory fsync at all).
func writeChildGroupFileLocked(path string, entries []childGroupEntry) {
	var sb strings.Builder
	for _, e := range entries {
		// Format: pgid generation [leaderStart [pid@start ...]] -- see
		// readChildGroupFileLocked, the authoritative parse-site comment.
		sb.WriteString(strconv.Itoa(e.PGID))
		sb.WriteByte(' ')
		sb.WriteString(e.Generation)
		if e.StartTime != "" {
			sb.WriteByte(' ')
			sb.WriteString(e.StartTime)
		}
		for _, w := range e.Witnesses {
			sb.WriteByte(' ')
			sb.WriteString(strconv.Itoa(w.PID))
			sb.WriteByte('@')
			sb.WriteString(w.StartTime)
		}
		sb.WriteByte('\n')
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		slog.Warn("session: failed to create temp file for child-group registry write", "path", path, "err", err)
		return
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename lands
	// -- a leftover .tmp-* file is harmless (never read by
	// readChildGroupFileLocked, which only ever opens the final path) but
	// there is no reason to leave it.
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.WriteString(sb.String()); err != nil {
		slog.Warn("session: failed to write temp file for child-group registry write", "path", path, "err", err)
		tmp.Close()
		return
	}
	if err := tmp.Sync(); err != nil {
		slog.Warn("session: failed to fsync temp file for child-group registry write", "path", path, "err", err)
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		slog.Warn("session: failed to close temp file for child-group registry write", "path", path, "err", err)
		return
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		// Non-fatal: os.CreateTemp already created the file 0o600, which is
		// strictly tighter than the 0o644 os.WriteFile used before this
		// change -- worth trying to match for consistency with the rest of
		// this package's permission model, but not worth abandoning the
		// write over.
		slog.Warn("session: failed to chmod temp file for child-group registry write", "path", path, "err", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Warn("session: failed to rename temp file into place for child-group registry write", "path", path, "err", err)
		return
	}
	succeeded = true

	// Fsync the parent directory so the rename itself (not just the file's
	// data, already forced to disk by tmp.Sync() above) survives a power
	// loss or unclean unmount -- see this function's own doc for why a
	// file-data fsync alone does not cover the directory-entry update. Best
	// effort and strictly additive: the write above already fully
	// succeeded (succeeded is already true), so a failure here is logged
	// and does not unwind or otherwise affect the write's own outcome.
	dirHandle, err := os.Open(dir)
	if err != nil {
		slog.Warn("session: failed to open registry directory for fsync", "dir", dir, "err", err)
		return
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		slog.Warn("session: failed to fsync registry directory after rename", "dir", dir, "err", err)
	}
}

// RemoveChildGroupRegistry deletes the registry file for sessionID
// unconditionally, regardless of contents. Called by sessions kill only
// after every entry it found has reached a CONFIRMED outcome (killed, or
// confirmed already dead) -- see KillRegisteredChildGroups, which now calls
// this only when nothing needs to be retained, and otherwise calls
// writeChildGroupFileLocked with the retained subset instead. Best-effort:
// a removal failure is logged only.
func RemoveChildGroupRegistry(dataDir, sessionID string) {
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	defer childGroupFileMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("session: failed to remove child-group registry file", "path", path, "err", err)
	}
}

// ChildGroupSweepResult reports what KillRegisteredChildGroups actually
// did, broken down by outcome, so a caller (sessions_kill.go) can tell an
// operator the difference between outcomes that must not be collapsed into
// a single "0 swept" report: nothing was ever registered; it was registered
// but a newer/different owner has since taken the session (or this rush
// process restarted) so nothing was touched; it was registered, still
// looked plausible, and was killed; or it was registered, attempted, and
// left in a genuinely unresolved state that a later retry might still
// clear.
type ChildGroupSweepResult struct {
	// Killed is the number of groups actually SIGKILLed.
	Killed int
	// GenerationMismatch is true when a registry entry's recorded
	// generation did not match victimGeneration -- the caller's own proven
	// target, not "whatever generation happens to be on disk right now".
	// An earlier version of this doc claimed these entries are "CONFIRMED
	// stale ... safe to drop" on the theory that a session's generation
	// only ever moves forward. That reasoning is exactly backwards when
	// the CALLER (not the entry) is the one reading a stale value: task
	// #591 blocker P0-2 (2026-08-19 static-follow-up review, reproduced by
	// running on real Linux) is the direct counter-proof -- an earlier
	// version of KillRegisteredChildGroups re-read "current" generation
	// from disk at sweep time, AFTER the victim holder had already died
	// and a brand-new owner had already acquired the lock and registered
	// its OWN live child group. That re-read observed the NEW owner's
	// generation as "current", which made the ACTUAL VICTIM's entries --
	// the live orphan tree this whole feature exists to reach -- mismatch
	// and get discarded right here, while the new owner's live process
	// group was signalled instead. With victimGeneration now fixed at the
	// moment contention was proven (see KillRegisteredChildGroups' doc),
	// a mismatch here means this specific entry does not belong to the
	// generation THIS sweep was invoked to clean up, so it is left
	// untouched in the registry rather than dropped -- a future sweep
	// invoked for that entry's own generation (if it is ever the victim
	// of one) is what may act on it, not this one.
	GenerationMismatch bool
	// Implausible counts entries whose generation matched but which were
	// CONFIRMED not to be the originally-registered process any more
	// (verifyGroupStillPlausibleOutcome reporting confirmedGone or
	// confirmedMismatch -- see that type) -- most likely already exited
	// and the pgid number reused for something else, or the whole group
	// has exited with nothing left to signal, so signalling it
	// would be unrelated to the original CLI-provider child. These are
	// also dropped: retrying against a pgid confirmed to be a DIFFERENT
	// process would never become correct by waiting.
	Implausible int
	// Retained counts entries whose generation matched but whose outcome
	// this attempt could not confirm one way or the other -- a
	// plausibility check that itself failed to complete (see
	// verifyGroupStillPlausibleOutcome's inconclusive case), or a kill
	// syscall that returned neither success nor a confirmed-already-gone
	// error (e.g. EPERM, or any error other than ESRCH). These entries are
	// written BACK to the registry, unchanged, so a second "sessions kill"
	// still has a durable pointer to retry against instead of the registry
	// being deleted out from under an unresolved failure.
	Retained int
}

// killpgFunc performs the actual kill(2) syscall used to signal a
// registered process group, as a package-level variable so tests can
// substitute a fake that returns an arbitrary error (EPERM, or anything
// else that is neither success nor ESRCH) -- deterministically exercising
// the retention branch below without needing root privileges or a real
// permission-denied process group, neither of which this codebase's test
// suite can reliably construct. Production always uses the real syscall;
// only tests in this package (which can see this unexported variable)
// override it, and always restore it via t.Cleanup. It is used for BOTH
// the real SIGKILL and the signal-0 group-existence probe in
// verifyGroupStillPlausibleOutcome's leader-gone branch, so a test fake
// must also expect (and handle) (-pgid, 0) probes.
var killpgFunc = syscall.Kill

// KillRegisteredChildGroups killpg's every process group registered for
// sessionID whose recorded generation matches victimGeneration and which
// still passes the plausibility check.
//
// victimGeneration MUST be an IMMUTABLE token the caller captured at a
// moment it proved the generation genuinely belongs to the holder whose
// entries are being swept -- either of two shapes, both implemented in
// internal/cmd/sessions_kill.go's probeThenKillHolder:
//
//   - A holder killed by this command: the token is read via
//     ReadLockGeneration(lockPath) in the busy branch, before
//     forceKillHolder ever signals anything, and threaded down through
//     forceKillHolder to here.
//   - A holder that had already crashed (or exited without cleanup) before
//     this command ever ran: the token is read BEFORE this command's own
//     TryAcquireSessionLock probe, because a successful acquire
//     immediately overwrites the ".gen" sidecar with the prober's own new
//     token -- reading it any later would read the PROBE's generation, not
//     the crashed holder's (task #602).
//
// This function deliberately does NOT read ReadLockGeneration itself: an
// earlier version did ("read fresh from disk, right here"), which was the
// root cause of a real defect (2026-08-19 static-follow-up review, task
// #591 blocker P0-2, confirmed by running on real Linux) -- on Unix,
// killing the holder PID releases the OS lock, so a new rush run
// --session <id> can acquire it and register its OWN child group under a
// NEW generation before this function ever runs. Re-reading "current"
// generation at sweep time reads that NEW owner's token, not the dead
// victim's, which both (a) signals the new owner's live process group and
// (b) discards the actual victim's entries as "generation mismatch".
// Passing the generation down as a value captured before ownership changed
// hands (or, in the crash case, before this function's own probe could
// overwrite the only remaining record of it) is the fix: the sweep target
// is fixed at the moment contention was proven -- or, when there was no
// contention because the holder already crashed, at the last moment its
// own generation was still readable -- never re-derived from whatever the
// lock file says when the sweep happens to run.
//
// CALLER MUST HOLD THE SESSION'S OS LOCK (session.SessionLock, acquired via
// TryAcquireSessionLock) across this ENTIRE call -- read, verify, kill, AND
// the write-back at the end. childGroupFileMu below only serializes this
// process's own goroutines; it provides no cross-process exclusion at all
// (see its doc comment). Without the OS lock held externally, a concurrent
// RegisterChildGroup from a live new owner can land between this
// function's read and its write-back, and get silently clobbered by the
// stale retained-snapshot write -- the same review's documented
// cross-process lost-update race. See sweepChildGroupsWithHeldLock and
// sweepChildGroupsUnderOwnLock in internal/cmd/sessions_kill.go, which are
// the only production callers and both acquire/hold the lock before
// calling in.
func KillRegisteredChildGroups(dataDir, sessionID, victimGeneration string) ChildGroupSweepResult {
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	entries := readChildGroupFileLocked(path)
	childGroupFileMu.Unlock()

	var result ChildGroupSweepResult
	if len(entries) == 0 {
		return result
	}

	if victimGeneration == "" {
		// No target: a caller must always pass the specific generation it
		// proved was the victim (see the doc comment above). An empty value
		// here is a programming error in the caller, not "sweep whatever is
		// current" -- treat it as matching nothing rather than guessing.
		result.GenerationMismatch = len(entries) > 0
		return result
	}
	var retained []childGroupEntry
	for _, e := range entries {
		if e.Generation != victimGeneration {
			// This entry does NOT belong to the generation this sweep was
			// invoked to clean up. It is NOT thereby proven stale -- it may
			// be an older, genuinely dead generation, OR it may belong to
			// the CURRENT live owner (who registered their own child group,
			// under their own generation, before or during this sweep --
			// possible even though the caller holds the OS lock across this
			// call, since the live owner's own RegisterChildGroup happened
			// BEFORE the sweep proved contention and killed the victim, and
			// is a record the current owner still needs for its OWN future
			// cleanup). Retain it unconditionally rather than drop it: only
			// a sweep whose OWN victim generation matches an entry may ever
			// decide that entry's fate. See KillRegisteredChildGroups' doc
			// comment for the P0-2 defect this replaces (a version of this
			// function once treated "generation differs from what I deem
			// current" as proof of permanent staleness, which is exactly
			// backwards when the caller itself might be looking at a stale
			// or otherwise non-authoritative target).
			result.GenerationMismatch = true
			retained = append(retained, e)
			continue
		}
		switch verifyGroupStillPlausibleOutcome(e.PGID, e.StartTime, e.Witnesses) {
		case plausibilityConfirmedGone, plausibilityConfirmedMismatch:
			// Confirmed: either the process is gone (Getpgid/ESRCH-shaped
			// failure) or a DIFFERENT process now holds this pgid (start
			// time no longer matches what was registered). Either way,
			// retrying later can never make this entry valid again.
			result.Implausible++
			continue
		case plausibilityInconclusive:
			// Could not determine liveness one way or the other (e.g. the
			// start-time re-read failed for a reason other than the
			// process being gone). Not safe to discard -- keep it for a
			// retry.
			result.Retained++
			retained = append(retained, e)
			continue
		}
		// plausibilityConfirmedLive falls through to the actual signal.
		killErr := killpgFunc(-e.PGID, syscall.SIGKILL)
		switch {
		case killErr == nil:
			result.Killed++
		case isErrno(killErr, syscall.ESRCH):
			// Confirmed: the group is already gone (a benign race between
			// the plausibility check above and this signal). Safe to drop.
			result.Implausible++
		default:
			// Neither confirmed killed nor confirmed already dead (EPERM,
			// or any other kill(2) failure) -- genuinely unresolved. Keep
			// it for a retry rather than silently losing the only durable
			// pointer to an unreached child tree.
			result.Retained++
			retained = append(retained, e)
		}
	}

	childGroupFileMu.Lock()
	defer childGroupFileMu.Unlock()
	if len(retained) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("session: failed to remove child-group registry file", "path", path, "err", err)
		}
	} else {
		writeChildGroupFileLocked(path, retained)
	}
	return result
}

// plausibilityOutcome is the fine-grained result of checking whether a
// registered pgid still looks like the process that was originally
// registered -- distinguishing "confirmed not our target" (safe to
// permanently discard the registry entry) from "could not tell" (must be
// retained for a retry). A plain bool (the shape verifyGroupStillPlausible
// used before task #591 part 3) could not make this distinction, which is
// what caused KillRegisteredChildGroups to discard entries it had not
// actually confirmed dead.
type plausibilityOutcome int

const (
	// plausibilityConfirmedLive: still exists, still a group leader, and
	// (if a start-time token was captured at registration) the start time
	// still matches -- OR the registered leader has been reaped but a
	// member witness captured at registration still exists with its
	// recorded identity (the historical-ownership proof that survives
	// leader exit; see verifyGroupStillPlausibleOutcome's leader-gone
	// branch). Safe to signal.
	plausibilityConfirmedLive plausibilityOutcome = iota
	// plausibilityConfirmedGone: the leader is gone AND no group with that
	// id remains (the leader-gone branch's signal-0 probe returned ESRCH),
	// or the pid exists but does not lead its own group -- the original
	// process-group leader is confirmed not reachable any more. Safe to
	// drop the entry.
	plausibilityConfirmedGone
	// plausibilityConfirmedMismatch: still exists and is still a group
	// leader, but the CURRENT start-time token does not match the one
	// captured at registration -- a DIFFERENT process has since reused
	// this exact pgid number. Safe to drop the entry: it will never again
	// refer to the process this registry entry was written for. This is
	// also how a reused pgid under a NEW live leader is safely
	// distinguished from the still-ours case.
	plausibilityConfirmedMismatch
	// plausibilityInconclusive: the group-leader check passed but the
	// start-time re-read itself could not be completed for a reason other
	// than the process being gone (e.g. a transient /proc read failure).
	// NOT safe to treat as either confirmed-live or confirmed-gone -- must
	// be retained for a retry rather than signalled or discarded.
	plausibilityInconclusive
)

// verifyGroupStillPlausibleOutcome re-checks, immediately before
// signalling, whether a registered pgid still looks like a signalable
// member of the originally-registered process group rather than a stale
// number the OS has since recycled for something unrelated -- and reports
// WHICH of those it confirmed, so the caller can decide whether a negative
// result is safe to discard or must be retained. See
// verifyGroupStillPlausible (the pre-#591-part-3 bool-returning wrapper,
// kept for any external caller that only needs a yes/no) for the original
// two-layer design this refines.
//
// Two branches, in order:
//
//  1. Leader branch -- syscall.Getpgid(pgid) succeeds. UNCHANGED
//     semantics: if pgid is no longer its own group's leader
//     (Getpgid(pgid) != pgid), plausibilityConfirmedGone. If
//     wantStartTime is empty (macOS: no start-time facility, would need a
//     sysctl/libproc binding not currently vendored),
//     plausibilityConfirmedLive on the leader check alone. Otherwise the
//     CURRENT start time (Linux: /proc/pid/stat field 22, see
//     startTimeToken) is re-read right here and must match EXACTLY: a
//     mismatch is plausibilityConfirmedMismatch (a different process now
//     owns this pgid -- confirmed, safe to drop), a re-read failing for a
//     reason other than confirmed death is plausibilityInconclusive
//     (deliberately NOT collapsed into confirmedGone, because this branch
//     already independently confirmed the pgid was a live group leader a
//     moment ago).
//
//  2. Leader-gone branch -- Getpgid returns an error, meaning no process
//     with this pid exists. That is the NORMAL state by sweep time: a
//     real "sessions kill" runs from a separate process after the holder
//     died, and the group's LEADER may long since have exited and been
//     reaped while non-leader members (grandchildren forked into the
//     group) still live -- a Getpgid-only check wrongly reported exactly
//     that state as plausibilityConfirmedGone and dropped the entry
//     without signalling. So this branch probes the GROUP itself with
//     killpgFunc(-pgid, 0) -- signal 0 is an existence/permission check
//     that sends nothing:
//
//     - nil, or EPERM (the group exists but holds a process we may not
//     signal -- the real SIGKILL below would surface EPERM into the
//     existing Retained path): plausibilityConfirmedLive, but ONLY if a
//     member witness still holds (below).
//     - ESRCH: no group with that id remains --
//     plausibilityConfirmedGone.
//     - Anything else: plausibilityInconclusive (retained for retry).
//
//     IDENTITY in this branch, and the options the 2026-10-18 review
//     weighed once the previous "identity by pid reservation" argument was
//     refuted by historical reuse: the registered group can exit
//     COMPLETELY, the pgid number can later be handed to an unrelated new
//     leader (pid == that number), which forks non-leader members and is
//     itself reaped -- Getpgid then fails again, the probe succeeds, and
//     the old code SIGKILLed those unrelated surviving members. The
//     options:
//
//     (a) record start-time identity of at least one MEMBER at
//     registration and require a surviving member with that identity --
//     the strongest available historical evidence, because a member's
//     (pid, start-time) pair is unique for the pid's lifetime and the
//     pair is captured while the registration is still verifiably ours;
//     (b) record the group's session id (SID) and compare -- rejected:
//     SID is inherited from the spawner (rush) or set by Setsid, is NOT
//     unique to this group (every CLI child of one rush shares it or has
//     its own, and a reused pgid under a new Setsid leader would carry a
//     new SID only by luck), so it narrows but does not establish
//     ownership;
//     (c) treat unverifiable identity as inconclusive, skip the kill --
//     the safe default, but alone it would forfeit the legitimate
//     cleanup of descendants of an exited leader even when we CAN prove
//     they are ours.
//
//     IMPLEMENTED: a hybrid of (a)+(c). Registration (RegisterChildGroup)
//     captures (pid, startTimeToken(pid)) pairs for the group's
//     non-leader members while the pgid is still a verified live leader.
//     At sweep time in THIS branch the group may be signalled only if at
//     least one recorded witness still exists with its recorded start
//     time; if no witnesses were recorded (platform without member
//     enumeration, no other members at registration, or an older
//     version's registry file) or none survives with matching identity,
//     the outcome is plausibilityInconclusive -- RETAINED, never
//     signalled. This preserves the intentional requirement that
//     descendants of an exited leader ARE cleaned when the evidence
//     holds (TestChildGroupRegistry_LeaderReaped_GroupStillSwept), and
//     makes the genuinely unprovable case conservatively inert.
//
// The residual TOCTOU windows -- between the Getpgid and the probe here,
// and between the probe (or the leader-branch checks) and the real
// SIGKILL in the caller -- are the same narrow, accepted class this file
// already documents (see probeThenKillHolder in
// internal/cmd/sessions_kill.go: "Given this is a manual rescue tool an
// operator runs deliberately, that structural fix is not implemented; the
// window is accepted as a narrow, known limitation"). The macOS
// no-start-time note above and this whole function were UNVERIFIED ON
// REAL LINUX/macOS FROM THIS (WINDOWS) DEVELOPMENT MACHINE -- the test
// suite, including the new leader-reaped regression test, now runs on
// Linux CI; see the package-level task notes for the run that confirms
// it.
func verifyGroupStillPlausibleOutcome(pgid int, wantStartTime string, witnesses []childGroupWitness) plausibilityOutcome {
	pg, err := syscall.Getpgid(pgid)
	if err != nil {
		// Leader gone: probe the group itself. See this function's doc
		// comment (leader-gone branch) for why a Getpgid failure alone is
		// NOT proof the registered group is gone -- the normal sweep-time
		// state is a reaped leader with non-leader members still living.
		probeErr := killpgFunc(-pgid, 0)
		switch {
		case probeErr == nil || isErrno(probeErr, syscall.EPERM):
			// The group still exists (EPERM: and holds a process we may
			// not signal -- still ours; the real SIGKILL surfaces EPERM
			// into the Retained path). But EXISTENCE is not IDENTITY: a
			// pgid number whose registered group fully exited can later be
			// reused by an unrelated new leader whose own forked members
			// keep the number alive after that leader is reaped -- exactly
			// the state this branch probes. The historical-ownership
			// witness captured at registration is the only evidence that
			// survives leader exit, so require at least one recorded
			// witness to still exist WITH its recorded start time before
			// signalling. No witnesses recorded (platform without member
			// enumeration, group had no other members at registration, or
			// a registry written by an older version) or none surviving
			// with matching identity => INCONCLUSIVE: retained, never
			// signalled. "Inconclusive, do not kill" is the safe outcome
			// whenever ownership cannot be established.
			if !anyWitnessStillHolds(pgid, witnesses) {
				return plausibilityInconclusive
			}
			return plausibilityConfirmedLive
		case isErrno(probeErr, syscall.ESRCH):
			// No group with that id remains: confirmed gone, safe to
			// drop.
			return plausibilityConfirmedGone
		default:
			// Neither confirmed alive nor confirmed gone: retained, not
			// discarded.
			return plausibilityInconclusive
		}
	}
	if pg != pgid {
		return plausibilityConfirmedGone
	}
	if wantStartTime == "" {
		// No start-time token was captured at registration (platform
		// without the facility, or the capture failed) -- fall back to
		// the group-leader check alone.
		return plausibilityConfirmedLive
	}
	gotStartTime, ok := startTimeToken(pgid)
	if !ok {
		// Could not re-read a start time now (process gone, or this
		// platform cannot read it) but COULD read a getpgid a moment
		// ago -- genuinely ambiguous: retained, not discarded.
		return plausibilityInconclusive
	}
	if gotStartTime != wantStartTime {
		return plausibilityConfirmedMismatch
	}
	return plausibilityConfirmedLive
}

// verifyGroupStillPlausible is the plain-bool convenience wrapper over
// verifyGroupStillPlausibleOutcome, kept for callers (existing tests) that
// only need a live/not-live answer and do not need to distinguish
// confirmed-dead from inconclusive.
func verifyGroupStillPlausible(pgid int, wantStartTime string) bool {
	return verifyGroupStillPlausibleOutcome(pgid, wantStartTime, nil) == plausibilityConfirmedLive
}

// anyWitnessStillHolds reports whether at least one recorded member
// witness still exists with the start-time token captured at registration
// -- the historical-ownership proof used by the leader-gone branch above.
// A witness pid that is gone, or whose current start time differs (its pid
// number was reused by a different process), fails; an empty witness list
// fails outright. The witness must also STILL BE A MEMBER of pgid: a process
// group number is not reused while any process remains in it, so a surviving
// member with the recorded birth identity proves the group is still the one
// we registered; a witness that moved to another group (setsid/setpgid) says
// nothing about a later group that reuses the number and must not authorise
// a signal.
func anyWitnessStillHolds(pgid int, witnesses []childGroupWitness) bool {
	for _, w := range witnesses {
		if pg, err := syscall.Getpgid(w.PID); err != nil || pg != pgid {
			continue
		}
		if got, ok := startTimeToken(w.PID); ok && got == w.StartTime {
			return true
		}
	}
	return false
}

// isErrno (used above to classify the SIGKILL syscall's own error) is
// defined once in kill_unix.go -- same package, same //go:build !windows
// constraint as this file, so it is reused directly rather than
// duplicated.
