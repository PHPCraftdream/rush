//go:build !windows

// Cross-process child process-group registry (#580), generation-checked.
//
// Why this exists: "crush sessions kill <id>" runs as a SEPARATE OS process
// from the crush instance that holds the session lock. It reads the holder
// PID from the lock file/sidecar and kills that one PID via KillProcess,
// which on Unix also killpg's the PID's OWN process group if that PID
// happens to be a leader of ITS group. But a CLI provider child
// (claude/gemini/codex/qwen, see internal/agent/cliprovider) is
// deliberately spawned as the leader of its OWN, DIFFERENT process group
// (procgroup_unix.go's configureChildProcessGroup / go-pty's Setsid),
// precisely so a stray signal to crush's terminal/group does not also kill
// it (see track_unix.go's doc comment and commit 59908cbe). That same
// design choice means killing the crush PID alone -- all "sessions kill"
// can reach through the lock file -- does NOT touch the CLI child's group.
//
// KillAllTrackedTrees (in track_unix.go) solves this for the IN-PROCESS
// crash-net path because that code runs inside the same crush process that
// holds the in-memory trackedGroups map. "sessions kill" runs in a separate
// process and has no access to that map, so the fix needs a durable,
// cross-process record.
//
// SECURITY: an earlier version of this file keyed that record purely by
// the crush process's numeric pid, under os.TempDir() (world-writable on
// most POSIX systems, 0755/0644). That was rejected in review for two
// independent reasons and replaced with the design below:
//
//  1. PID reuse: a crush process that crashes never runs
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
//     CURRENT on-disk generation for that session still matches the
//     recorded one -- proving the lock has not been released and
//     re-acquired (by a restart, or a different session reusing the id
//     after the first died and was reaped) since registration. This is the
//     exact mechanism internal/session/lock.go's own clearHolderMetadata
//     uses to guard its own destructive cleanup; see ReadLockGeneration's
//     doc for why a bare pid is not an identity but a (session id,
//     generation) pair is.
//   - Every pgid is re-validated for plausibility immediately before
//     signalling (see verifyGroupStillPlausible): still a live process
//     group leader. This narrows, but as documented on
//     verifyGroupStillPlausible, does not fully close, the residual
//     pid-reuse race between validation and the signal itself.
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
// provider child process groups a session's crush process registered,
// alongside the lock generation token that was current when each entry was
// written. Lives next to the session lock file itself
// (dataDir/locks/session-<id>.lock), not in a shared world-writable
// directory: same permission model as the PID/generation sidecars already
// written there by internal/session/lock.go.
func childGroupRegistryPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".childgroups")
}

// childGroupFileMu serializes this process's own read-modify-write cycles
// against its own session's registry file. It does not, and cannot,
// protect against a concurrent writer for the SAME session in another
// process -- but the session lock itself already guarantees only one
// process holds a given session at a time, so there is only ever one
// legitimate writer for a given (dataDir, sessionID) registry at once.
var childGroupFileMu sync.Mutex

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
}

// RegisterChildGroup durably records that the crush process currently
// holding sessionID's lock is responsible for tearing down the process
// group led by pgid if it is killed from outside without running its own
// cleanup. Call only for a confirmed process-group leader (pgid == its own
// pgid) -- see TrackProcessTree's identical constraint, which this is meant
// to be called alongside.
//
// generation MUST be the value session.ReadLockGeneration(lockPath) reports
// for THIS session at (or immediately before) registration time -- see
// ReadLockGeneration's doc for why a stale or empty generation makes this
// entry permanently unusable by design (KillRegisteredChildGroups requires
// an exact, non-empty match against the CURRENT on-disk generation).
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
	existing := readChildGroupFileLocked(path)
	for i, e := range existing {
		if e.PGID == pgid && e.Generation == generation {
			// Re-registering the same (pgid, generation): refresh the
			// start-time token in case it was unavailable the first time
			// (e.g. captured before the child fully started) and is
			// available now.
			if startTime != "" && e.StartTime == "" {
				existing[i].StartTime = startTime
				writeChildGroupFileLocked(path, existing)
			}
			return
		}
	}
	existing = append(existing, childGroupEntry{PGID: pgid, Generation: generation, StartTime: startTime})
	writeChildGroupFileLocked(path, existing)
}

// UnregisterChildGroup removes pgid from sessionID's durable registry,
// mirroring UntrackProcessTree's in-memory forget. Safe to call for a pgid
// that was never registered. When the registry file becomes empty it is
// removed entirely so a long-lived crush process that starts and stops many
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
// best-effort augmentation, never a correctness dependency. Format: one
// pgid-space-generation pair per line.
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
		if len(fields) == 3 {
			startTime = strings.TrimSpace(fields[2])
		}
		entries = append(entries, childGroupEntry{PGID: pgid, Generation: gen, StartTime: startTime})
	}
	return entries
}

// writeChildGroupFileLocked overwrites the registry file for one session.
// Caller must hold childGroupFileMu. Best-effort: a write failure is logged
// only.
func writeChildGroupFileLocked(path string, entries []childGroupEntry) {
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(strconv.Itoa(e.PGID))
		sb.WriteByte(' ')
		sb.WriteString(e.Generation)
		if e.StartTime != "" {
			sb.WriteByte(' ')
			sb.WriteString(e.StartTime)
		}
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		slog.Warn("session: failed to write child-group registry file", "path", path, "err", err)
	}
}

// RemoveChildGroupRegistry deletes the registry file for sessionID
// unconditionally, regardless of contents. Called by sessions kill after
// acting on (or attempting to act on) every group it found, so a stale file
// cannot mislead a later, unrelated sessions kill invocation. Best-effort:
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
// operator the difference between three outcomes that must not be
// collapsed into a single "0 swept" report: nothing was ever registered;
// it was registered but a newer/different owner has since taken the
// session (or this crush process restarted) so nothing was touched; or it
// was registered, still looked plausible, and was killed.
type ChildGroupSweepResult struct {
	// Killed is the number of groups actually SIGKILLed.
	Killed int
	// GenerationMismatch is true when a registry file existed but its
	// recorded generation did not match the CURRENT on-disk lock
	// generation -- the session has a different (or no) live owner than
	// the one that registered these groups, so nothing was signalled.
	GenerationMismatch bool
	// Implausible counts entries whose generation matched but which
	// failed the pre-signal plausibility check (see
	// verifyGroupStillPlausible) -- most likely already exited and the
	// pgid number reused for something else, so signalling it would be
	// unrelated to the original CLI-provider child.
	Implausible int
}

// KillRegisteredChildGroups killpg's every process group registered for
// sessionID whose recorded generation still matches the session's CURRENT
// lock generation (read fresh from disk, right here, via
// ReadLockGeneration) and which still passes verifyGroupStillPlausible. It
// then removes the registry file regardless of outcome, since a registry
// that no longer matches the live generation can never become valid again
// (a session's generation only moves forward).
//
// lockPath is the session's own lock file path (sessions_kill.go already
// computes this before calling in); dataDir/sessionID identify the
// registry file itself.
func KillRegisteredChildGroups(dataDir, sessionID, lockPath string) ChildGroupSweepResult {
	path := childGroupRegistryPath(dataDir, sessionID)
	childGroupFileMu.Lock()
	entries := readChildGroupFileLocked(path)
	childGroupFileMu.Unlock()

	var result ChildGroupSweepResult
	if len(entries) == 0 {
		return result
	}

	currentGen := ReadLockGeneration(lockPath)
	for _, e := range entries {
		if currentGen == "" || e.Generation != currentGen {
			result.GenerationMismatch = true
			continue
		}
		if !verifyGroupStillPlausible(e.PGID, e.StartTime) {
			result.Implausible++
			continue
		}
		if err := syscall.Kill(-e.PGID, syscall.SIGKILL); err == nil {
			result.Killed++
		} else {
			result.Implausible++
		}
	}

	RemoveChildGroupRegistry(dataDir, sessionID)
	return result
}

// verifyGroupStillPlausible re-checks, immediately before signalling, that
// pgid still looks like a real process group leader rather than a stale
// number the OS has since recycled for something unrelated.
//
// Two layers, in order:
//
//  1. syscall.Getpgid(pgid) == pgid: still exists and is still the leader
//     of its own group. Rules out "no such process" and "exists but is no
//     longer (or never was, for a freshly recycled number) a leader".
//
//  2. If wantStartTime is non-empty (Linux: captured from
//     /proc/pid/stat field 22 at registration time, see startTimeToken),
//     the CURRENT start time for pgid is re-read right here and must
//     match EXACTLY. A process's start time is fixed at exec and the
//     kernel does not reuse a pid for a new process without first fully
//     reaping the old one, so an unrelated process that recycled this
//     exact pgid number will almost certainly have a different start
//     time; this closes the pid-reuse TOCTOU window on Linux to the
//     residual case where the kernel recycles the pid AND field 22
//     happens to collide, which does not happen in practice.
//
// On platforms without a start-time facility this codebase otherwise
// depends on (macOS: no /proc, would need a sysctl/libproc binding not
// currently vendored), wantStartTime is always "" and this function falls
// back to layer 1 alone -- the same narrow, accepted TOCTOU window
// documented on probeThenKillHolder in internal/cmd/sessions_kill.go for
// the plain-pid case ("Given this is a manual rescue tool an operator runs
// deliberately, that structural fix is not implemented; the window is
// accepted as a narrow, known limitation"). Combined with the generation
// check the caller already performed (which independently rules out the
// far more likely failure mode of a stale registry from a dead or
// superseded crush instance), the residual risk on macOS is a live,
// actively running, freshly-spawned group leader recycling the exact pgid
// in a multi-millisecond window -- materially narrower than the
// unconditional kill-on-read this whole file replaces, and on Linux closed
// by layer 2 above. THIS IS UNVERIFIED ON REAL LINUX/macOS FROM THIS
// (Windows) DEVELOPMENT MACHINE -- see the package-level task notes for
// the exact run that would confirm it.
func verifyGroupStillPlausible(pgid int, wantStartTime string) bool {
	pg, err := syscall.Getpgid(pgid)
	if err != nil {
		return false
	}
	if pg != pgid {
		return false
	}
	if wantStartTime == "" {
		// No start-time token was captured at registration (platform
		// without the facility, or the capture failed) -- fall back to
		// the group-leader check alone.
		return true
	}
	gotStartTime, ok := startTimeToken(pgid)
	if !ok {
		// Could not re-read a start time now (process gone, or this
		// platform cannot read it) but COULD read a getpgid a moment
		// ago -- treat as inconclusive and fail closed rather than
		// trusting the weaker check alone once a stronger one was
		// available at registration time.
		return false
	}
	return gotStartTime == wantStartTime
}
