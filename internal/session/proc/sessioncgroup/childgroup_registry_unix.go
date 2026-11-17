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
// it (see the track_unix.go doc comment in internal/kit/sys/proctrack and commit 59908cbe). That same
// design choice means killing the rush PID alone -- all "sessions kill"
// can reach through the lock file -- does NOT touch the CLI child's group.
//
// proctrack.KillAllTrackedTrees (internal/kit/sys/proctrack/track_unix.go)
// solves this for the IN-PROCESS crash-net path because that code runs
// inside the same rush process that holds proctrack's in-memory group map. "sessions kill" runs in a separate
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
package sessioncgroup

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
)

// childGroupRegistryPath returns the sidecar file recording the CLI
// provider child process groups a session's rush process registered,
// alongside the lock generation token that was current when each entry was
// written. Lives next to the session lock file itself
// (dataDir/locks/session-<id>.lock), not in a shared world-writable
// directory: same permission model as the PID/generation sidecars already
// written there by internal/session/lock.go.
func childGroupRegistryPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sessionlock.SanitiseSessionID(sessionID)+".childgroups")
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
// pgid) -- see proctrack.TrackProcessTree's identical constraint, which this is meant
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
// mirroring proctrack.UntrackProcessTree's in-memory forget. Safe to call for a pgid
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
