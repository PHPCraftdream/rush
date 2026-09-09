package session

// Reading a lock from the outside: path derivation, the PID and generation sidecars, and the read-only inspection surface other processes use to judge whether a holder is still alive. Split out of lock.go when the 1000-line file limit landed; lock.go keeps acquisition, release and the heartbeat.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func sanitiseSessionID(id string) string {
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

func readLockHolderPID(path string) int {
	pid, _ := readLockFile(path)
	return pid
}

// SessionLockPath returns the on-disk path of sessionID's lock file under
// dataDir, without creating any directory or touching the filesystem.
// Exported so callers outside this package that need to locate (but not
// necessarily acquire) a session's lock file -- e.g.
// internal/agent/cliprovider's child-process-group registry, which reads
// the lock's generation token via ReadLockGeneration -- can compute the
// exact same path TryAcquireSessionLock itself uses, without duplicating
// sanitiseSessionID's escaping rules.
func SessionLockPath(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".lock")
}

// ReadLockPID is the exported variant of readLockHolderPID, used by
// `rush sessions kill` / `reset --force` to read the PID off a lock
// file without having to re-implement the multi-line parse (the file
// stores PID on line 1, optional timeout in seconds on line 2).
func ReadLockPID(path string) int {
	return readLockHolderPID(path)
}

// ReadLockTimeoutSec returns the timeout-in-seconds stored on the second line
// of a lock file (written by TryAcquireSessionLockWithTimeout). Returns 0 if
// not present or unreadable — backward compatible.
func ReadLockTimeoutSec(path string) int64 {
	_, t := readLockFile(path)
	return t
}

// ReadLockGeneration returns the generation token currently stamped in
// path's ".gen" sidecar (see writeGenerationSidecar / the SessionLock
// generation field), or "" if the sidecar is missing or unreadable. This is
// the exported counterpart of the same read clearHolderMetadata performs
// internally, made available to callers outside this package that need to
// prove a piece of state they recorded still belongs to the CURRENT lock
// holder rather than a since-superseded or since-dead one.
//
// Used by internal/agent/cliprovider's child-process-group registry
// (see internal/session/childgroup_registry_unix.go): the registry records
// this token alongside the pgids it tracks, and `rush sessions kill`
// refuses to signal any of them unless this function, read again at kill
// time, still returns the SAME token — proving the lock has not been
// released and re-acquired (by this rush process restarting, or by an
// entirely different one after a PID/session reuse) since the registration
// was written.
func ReadLockGeneration(path string) string {
	data, err := os.ReadFile(generationSidecarPath(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// LockState describes the inter-process ownership of a session: who holds
// the lock right now, and how fresh the heartbeat is. Used by the web
// server's session list to surface "Owned externally by PID N" so a tab
// opened on a session that's being driven from another process renders
// read-only.
type LockState struct {
	Exists bool          // lock file is present on disk
	PID    int           // PID written into the lock file (0 if unreadable)
	Age    time.Duration // time since the last heartbeat touch (mtime)
	// Live reports whether the lock holder is still (probably) alive.
	// Primarily Age < liveThreshold — a healthy owner is touching the
	// heartbeat. But per task #228, mtime alone is not sufficient: the
	// heartbeat's mtime touch is gated on real RecordActivity() calls
	// (task #213/#214), and the stream watchdog that drives those calls
	// during a tool-in-flight only fires roughly every 30s (task #222,
	// streamWatchdogTick) — larger than the 20s liveThreshold callers
	// typically pass. That leaves a real window where a perfectly healthy
	// session blocked on one long tool call looks stale by mtime alone.
	// When mtime looks stale, InspectSessionLock falls back to a real PID
	// liveness probe (session.IsProcessAlive) before concluding the
	// holder is dead — same principle sessions_watch.go's
	// combinedLockLiveness already established for the `sessions watch`
	// consumer. That PID-liveness fallback is itself bounded by
	// maxPidFallbackAge (60m): once a stale lock is older than that, a
	// live-looking PID is no longer trusted and Live falls back to
	// mtime-only, so an OS PID reused by an unrelated process long after
	// the real holder died can't pin Live: true forever.
	Live bool
	// StatErr is non-nil when the lock file's existence could not be
	// determined at all (permission denied, I/O error, unreachable path
	// component, ...). It is nil both when the file exists and when it
	// verifiably does not (os.IsNotExist) — "absent" and "could not check"
	// are different answers. Display consumers intentionally ignore this
	// and stay fail-open (Exists/Live both false); the startup recovery
	// sweep (app.recoverInterruptedTurns) reads it to fail CLOSED —
	// StatErr != nil is treated as "possibly live" so the sweep skips
	// the session instead of clobbering it.
	StatErr error
}

// InspectSessionLock reads the lock file for `sessionID` under `dataDir`
// without acquiring it. Safe to call from any process — no side effects.
// `liveThreshold` defines how fresh the heartbeat must be to count as
// "live" by mtime alone (callers typically pass 20s — the same expiry the
// heartbeat loop uses; see TryAcquireSessionLock comments).
//
// mtime freshness is only the fast path, not the whole story — see the
// Live field's doc comment on LockState for why: task #228 found that a
// healthy, tool-busy session can transiently look stale by mtime alone
// (heartbeat is activity-gated, the stream watchdog's tick that supplies
// that activity can be slower than liveThreshold). When mtime already
// looks stale, this falls back to probing whether the recorded PID is
// still a live OS process (session.IsProcessAlive) before reporting
// Live: false — mirroring the same "don't trust mtime alone" fallback
// sessions_watch.go's combinedLockLiveness already uses. The PID probe is
// only attempted once mtime is already stale, so the common case (mtime
// fresh) pays no extra cost.
//
// The PID-fallback itself is bounded by maxPidFallbackAge (60 minutes):
// once the lock's mtime is older than that, a live-looking PID is no
// longer trusted and Live falls back to mtime-only, even if the recorded
// PID happens to currently belong to some live OS process. Without this
// bound, a stale lock left behind by a killed/crashed holder would
// report Live: true forever the moment the OS happened to recycle that
// exact PID number for an unrelated process — turning a false-negative
// fix (task #228) into an unbounded false-positive. See maxPidFallbackAge's
// doc comment for how the bound was chosen.
func InspectSessionLock(dataDir, sessionID string, liveThreshold time.Duration) LockState {
	if dataDir == "" || sessionID == "" {
		return LockState{}
	}
	path := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionID(sessionID)+".lock")
	st, err := os.Stat(path)
	if err != nil {
		// "Could not check" (any stat error other than verifiable absence)
		// must not be indistinguishable from "verifiably absent". Both still
		// return the same fail-open zero state below — Exists:false, Live:false
		// — so display consumers are unaffected; only StatErr carries the
		// distinction. The startup recovery sweep reads it to fail closed.
		if !os.IsNotExist(err) {
			return LockState{StatErr: err}
		}
		return LockState{}
	}
	pid := ReadLockPID(path)
	age := time.Since(st.ModTime())
	live := age < liveThreshold
	if !live && pid > 0 && age < maxPidFallbackAge {
		// mtime looks stale — task #228: fall back to a real PID liveness
		// check before concluding the holder is dead, since the
		// heartbeat's mtime touch is now gated on real activity and a
		// healthy session blocked on one long tool call can look stale
		// here even though it is still genuinely running. Bounded by
		// maxPidFallbackAge (task #235) so a PID the OS has since recycled
		// for an unrelated process can't keep reporting Live: true forever
		// once the lock is old enough that no genuinely healthy holder
		// could still be running.
		live = IsProcessAlive(pid)
	}
	return LockState{
		Exists: true,
		PID:    pid,
		Age:    age,
		Live:   live,
	}
}

// pidSidecarPath returns the companion, never-locked file that carries a
// live copy of the PID/timeout normally stamped into the lock file itself.
// See readLockFile's doc comment for why this sidecar exists.
func pidSidecarPath(lockPath string) string {
	return lockPath + ".pid"
}

// generationSidecarPath returns the companion file that carries a unique
// generation token for each acquire instance. Used to prevent stale cleanup
// goroutines from clobbering a new owner's metadata.
func generationSidecarPath(lockPath string) string {
	return lockPath + ".gen"
}

// writePIDSidecar (re)writes the sidecar file next to a session lock with
// the holder's PID and (optionally) its --timeout budget in seconds,
// mirroring what's stamped into the lock file itself. Unlike the lock
// file, this file is opened, written, and closed immediately — no lock is
// ever held on it — so any other process can read it with a plain
// os.ReadFile at any time, including while the lock file's own mandatory
// Windows range-lock makes IT unreadable (see readLockFile).
//
// Best-effort: a write failure only degrades diagnosability (PID shown as
// 0 to a concurrent reader) and is logged, never returned as an error —
// the OS lock on the primary file remains the sole source of truth for
// correctness, exactly like the mtime heartbeat.
func writePIDSidecar(lockPath string, pid int, timeoutSec int64) {
	content := strconv.Itoa(pid) + "\n"
	if timeoutSec > 0 {
		content += strconv.FormatInt(timeoutSec, 10) + "\n"
	}
	if err := os.WriteFile(pidSidecarPath(lockPath), []byte(content), 0o644); err != nil {
		slog.Warn("session lock: failed to write PID sidecar", "path", lockPath, "err", err)
	}
}

// writeGenerationSidecar writes the unique generation token for this acquire
// instance. Called immediately after successful OS lock acquisition. Best-effort:
// a write failure only degrades the protection against metadata clobber and is
// logged, never returned as an error — the OS lock remains the sole source of
// truth for correctness.
func writeGenerationSidecar(lockPath, generation string) {
	if err := os.WriteFile(generationSidecarPath(lockPath), []byte(generation), 0o644); err != nil {
		slog.Warn("session lock: failed to write generation sidecar", "path", lockPath, "err", err)
	}
}

// readLockFile returns (PID, timeoutSec) for a session lock. Both default
// to 0 on any parse error — backward compatible with old one-line files.
//
// Windows note: tryLockFile takes a LockFileEx exclusive lock over the
// WHOLE file for the entire lifetime of the holder. Unlike POSIX advisory
// locks, Windows enforces this as a MANDATORY lock — any plain read from a
// different handle/process into that byte range (which is exactly what
// os.ReadFile does) fails with a sharing/lock violation for as long as the
// holder is alive, not just during a brief write race. So reading the lock
// file itself would return (0, 0) for an ACTIVELY RUNNING session on
// Windows — not because the PID is unknown, but because the mandatory lock
// blocks the read outright. `sessions kill` hitting exactly this case (a
// genuinely live holder) used to be unable to read a PID to kill at all.
//
// To fix that, TryAcquireSessionLock also stamps the same PID/timeout into
// a sidecar file (pidSidecarPath) that is never locked. Prefer that
// sidecar; fall back to the primary lock file only when no sidecar exists
// (an old lock file predating this change, or — in tests — a lock file
// written directly via os.WriteFile rather than through
// TryAcquireSessionLock).
func readLockFile(path string) (int, int64) {
	bts, err := os.ReadFile(pidSidecarPath(path))
	if err != nil {
		bts, err = os.ReadFile(path)
		if err != nil {
			return 0, 0
		}
	}
	lines := strings.Split(strings.TrimSpace(string(bts)), "\n")
	pid := 0
	var timeoutSec int64
	if len(lines) >= 1 {
		pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	}
	if len(lines) >= 2 {
		timeoutSec, _ = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	}
	return pid, timeoutSec
}
