package cmd

// `rush sessions compact` (task #1161): explicit SQLite VACUUM of the
// shared rush.db. Deletions (`sessions gc`/`purge`, `sessions delete`)
// hand pages back to SQLite's freelist but never shrink the file; this
// command rebuilds it. Nothing runs automatically — every invocation is
// an explicit operator request.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCompactCmd = &cobra.Command{
	Use:   "compact",
	Short: "Reclaim free space in rush.db (SQLite VACUUM)",
	Long: `Rebuild rush.db with SQLite VACUUM to return freed pages to the
filesystem.

Deleting sessions ("sessions gc", "sessions purge", "sessions delete")
moves their pages to the database's freelist; the rush.db file itself does
not shrink. This command rebuilds the file in place so its size matches
the live data. It never runs automatically: not on startup, not on a
timer, and not as a side effect of gc/purge.

While it runs the database is locked for writes for the duration of the
operation (typically seconds for a database whose live data is tens of
MB). A migrate.lock is held for the whole run, so new rush processes
(web server, "rush run") started against the same data directory wait at
startup until the compaction finishes. Refusals:

  - live rush hosts (hosts/*.lock probes) or live session locks
    (locks/session-*.lock inspected like "sessions locks") refuse the run
    and list the offenders; --force skips ONLY these two checks;
  - the startup migrate.lock and a busy WAL checkpoint (another
    connection with an open transaction) are NEVER bypassed, not even
    with --force — at either of them a VACUUM is guaranteed to collide
    with someone else's writes;
  - free disk space is checked in advance on both the data volume and the
    temp volume: roughly 2x the live data plus the current WAL plus a
    64 MB reserve is needed.

Known blind spot: a rush process that holds an open database pool but
registers no host lock and no session lock is invisible to the liveness
gates. As of this writing the web server registers its host lock lazily —
only when its async job store first claims work (internal/app/app.go,
AsyncJobStore) — so an IDLE web server with no claimed jobs is exactly
such an invisible process. SQLite still protects the file itself (the
idle server's writers get SQLITE_BUSY after the 30s busy timeout), and
the checkpoint gate catches an open transaction, but a wedged idle server
may leave the compaction unable to truncate the WAL. Stop other rush
processes on this data directory for a clean run.

Use --dry-run to print the current size, freelist, estimated result and
the liveness-gate verdicts without compacting (the only side effect is a
WAL checkpoint truncation). Use --json for a single machine-readable
summary object. Use --force to compact despite live hosts/session locks
(documented above: their writers may fail with SQLITE_BUSY).`,
	Example: `
# What would be reclaimed, without touching anything
rush sessions compact --dry-run

# Compact the database
rush sessions compact

# Machine-readable summary
rush sessions compact --json
  `,
	RunE: sessionsCompactCmdRun,
}

// Test seams (package vars, per the task's instruction): thresholds are
// substitutable so tests exercise the real refusal paths on multi-MB test
// databases instead of gigabyte ones.
var (
	// compactMinReclaimBytes: below this much freelist the command prints
	// "nothing to reclaim" and exits 0 without running VACUUM.
	compactMinReclaimBytes = int64(1 << 20)

	// compactReserveBytes computes the free disk space a compaction needs:
	// a temp copy of the live pages (VACUUM's temp store) plus the current
	// WAL plus a fixed reserve.
	compactReserveBytes = func(liveBytes, walBytes int64) int64 {
		return 2*liveBytes + walBytes + 64<<20
	}
)

// compactResult is the --json summary object.
type compactResult struct {
	Path           string `json:"path"`
	DryRun         bool   `json:"dry_run"`
	BeforeBytes    int64  `json:"before_bytes"`
	BeforeFree     int64  `json:"before_free_bytes"`
	AfterBytes     int64  `json:"after_bytes"`
	AfterFree      int64  `json:"after_free_bytes"`
	ReclaimedBytes int64  `json:"reclaimed_bytes"`
	DurationMS     int64  `json:"duration_ms"`
}

func sessionsCompactCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")
	asJSON, _ := cmd.Flags().GetBool("json")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	ctx := cmd.Context()
	dataDir := a.Config().Options.DataDirectory
	dbPath := filepath.Join(dataDir, "rush.db")
	conn := a.DB()

	before, err := db.Stats(ctx, conn)
	if err != nil {
		return fmt.Errorf("read database stats: %w", err)
	}

	result := compactResult{
		Path:        dbPath,
		DryRun:      dryRun,
		BeforeBytes: before.Bytes,
		BeforeFree:  before.FreeBytes,
	}

	// First line of the human output is always the database path: with the
	// shared data directory (#1143) it is not necessarily the .rush next
	// to the cwd.
	if !asJSON {
		fmt.Printf("database: %s\n", dbPath)
	}

	if before.FreeBytes < compactMinReclaimBytes {
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		fmt.Println("nothing to reclaim")
		return nil
	}

	// Gates 3-4: live hosts and live session locks. Computed once; in a
	// real run they refuse (unless --force), in a dry run they are only
	// reported.
	hostOffenders, hostErr := compactLiveHosts(dataDir)
	sessionOffenders, sessionErr := compactLiveSessionLocks(dataDir)
	if !dryRun && !force {
		if err := compactGateRefusal(hostErr, hostOffenders, sessionErr, sessionOffenders); err != nil {
			return err
		}
	}

	// Gate 5: a busy checkpoint is the last SQLite-level check. It is
	// never bypassed — not in a real run, not with --force. In a dry run
	// the checkpoint's only effect is truncating the -wal sidecar; the
	// main database file is not modified.
	busyErr := db.CheckpointTruncate(ctx, conn)

	live := before.LiveBytes()
	need := compactReserveBytes(live, before.WALBytes)
	freeData, totalData, dfErr := db.DiskFreeBytes(dataDir)
	if dfErr == nil {
		// The VACUUM's temp store lives in the temp directory (temp_store
		// is switched to FILE for the run), so that volume needs room too.
		var freeTemp, totalTemp uint64
		freeTemp, totalTemp, dfErr = db.DiskFreeBytes(os.TempDir())
		if freeTemp < freeData {
			freeData = freeTemp
		}
		if totalTemp < totalData {
			totalData = totalTemp
		}
	}

	if dryRun {
		if !asJSON {
			fmt.Printf("before: %s (free pages %s, %d%%)\n",
				humanBytes(before.Bytes), humanBytes(before.FreeBytes), percentOf(before.FreeBytes, before.Bytes))
			fmt.Printf("after (estimated): %s (live data)\n", humanBytes(live))
			if dfErr != nil {
				fmt.Printf("disk space: unavailable (%v)\n", dfErr)
			} else {
				fmt.Printf("disk space needed: %s; free: %s\n", humanBytes(need), humanBytes(int64(freeData)))
			}
			fmt.Printf("gates: hosts %s; session locks %s; wal checkpoint %s\n",
				compactGateVerdict(hostErr, hostOffenders),
				compactGateVerdict(sessionErr, sessionOffenders),
				compactGateVerdict(busyErr, nil))
		} else {
			result.AfterBytes = live
			if dfErr == nil {
				result.AfterFree = int64(freeData)
			}
			if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
				return err
			}
		}
		return nil
	}

	if busyErr != nil {
		return fmt.Errorf("refusing to compact: %w", busyErr)
	}
	if err := compactSpaceRefusal(dfErr, need, freeData, totalData); err != nil {
		return err
	}

	// Gate 2: hold migrate.lock for the rest of the run so new rush
	// processes wait at startup instead of writing mid-VACUUM. Strictly
	// AFTER Connect (setupApp above): connect() itself takes this lock,
	// so grabbing it first would deadlock against our own startup. Never
	// bypassed, not even with --force.
	migrateLock, err := filelock.TryAcquireFileLock(filepath.Join(dataDir, "migrate.lock"))
	if err != nil {
		return fmt.Errorf("refusing to compact: migrate.lock is held (a rush process may be starting on this database); retry when it is free: %w", err)
	}
	defer migrateLock.Release()

	start := time.Now()
	after, err := db.Compact(ctx, conn)
	if err != nil {
		return fmt.Errorf("compact failed (the database is left unchanged): %w", err)
	}
	elapsed := time.Since(start)

	result.AfterBytes = after.Bytes
	result.AfterFree = after.FreeBytes
	result.ReclaimedBytes = before.Bytes - after.Bytes
	result.DurationMS = elapsed.Milliseconds()

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	fmt.Printf("before: %s (free pages %s, %d%%)\n",
		humanBytes(before.Bytes), humanBytes(before.FreeBytes), percentOf(before.FreeBytes, before.Bytes))
	fmt.Printf("after: %s (free pages %s)\n", humanBytes(after.Bytes), humanBytes(after.FreeBytes))
	fmt.Printf("reclaimed %s in %.1fs\n", humanBytes(result.ReclaimedBytes), elapsed.Seconds())
	return nil
}

// compactGateRefusal turns gate 3-4 findings into the documented refusal
// error. hostErr/sessionErr are the scan errors themselves (a scan that
// could not run is also a refusal — an unverifiable gate is not a pass).
func compactGateRefusal(hostErr error, hosts []string, sessionErr error, sessions []string) error {
	var parts []string
	if hostErr != nil {
		parts = append(parts, fmt.Sprintf("host locks could not be probed: %v", hostErr))
	} else if len(hosts) > 0 {
		parts = append(parts, fmt.Sprintf("%d live rush host(s): %s", len(hosts), strings.Join(hosts, ", ")))
	}
	if sessionErr != nil {
		parts = append(parts, fmt.Sprintf("session locks could not be inspected: %v", sessionErr))
	} else if len(sessions) > 0 {
		parts = append(parts, fmt.Sprintf("%d live session lock(s): %s", len(sessions), strings.Join(sessions, ", ")))
	}
	if len(parts) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to compact: %s; stop them or re-run with --force", strings.Join(parts, "; "))
}

func compactGateVerdict(err error, offenders []string) string {
	switch {
	case err != nil:
		return "UNKNOWN (" + err.Error() + ")"
	case len(offenders) > 0:
		return fmt.Sprintf("LIVE (%s)", strings.Join(offenders, ", "))
	default:
		return "clear"
	}
}

// compactSpaceRefusal refuses when the free-disk-space probe failed or
// came back short of the reservation, with the numbers in the message.
func compactSpaceRefusal(probeErr error, need int64, free uint64, total uint64) error {
	if probeErr != nil {
		return fmt.Errorf("refusing to compact: cannot verify free disk space: %w", probeErr)
	}
	if int64(free) < need {
		return fmt.Errorf("refusing to compact: not enough free disk space: need ~%s (2x live data + wal + reserve), free %s of %s",
			humanBytes(need), humanBytes(int64(free)), humanBytes(int64(total)))
	}
	return nil
}

// compactLiveHosts probes every hosts/*.lock under dataDir with the
// shared, non-acquiring probe: Alive and Unknown both refuse — an
// unverifiable host could be mid-write.
func compactLiveHosts(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(session.HostsDir(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var offenders []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lock" {
			continue
		}
		path := filepath.Join(session.HostsDir(dataDir), entry.Name())
		status, err := session.ProbeHostLockShared(path)
		if err != nil {
			offenders = append(offenders, fmt.Sprintf("%s (probe error: %v)", entry.Name(), err))
			continue
		}
		if status != session.HostStatusDead {
			offenders = append(offenders, entry.Name())
		}
	}
	return offenders, nil
}

// compactLiveSessionLocks inspects every locks/session-*.lock under
// dataDir exactly like `sessions locks` does (InspectSessionLock, with its
// bounded PID fallback): a lock whose holder is live refuses the run.
func compactLiveSessionLocks(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "locks"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var offenders []string
	for _, entry := range entries {
		if entry.IsDir() || !fileStemMatches(entry.Name(), "session-", ".lock") {
			continue
		}
		id := trimAffixes(entry.Name(), "session-", ".lock")
		state := session.InspectSessionLock(dataDir, id, session.LockStaleDuration)
		if state.StatErr != nil {
			offenders = append(offenders, fmt.Sprintf("%s (stat error: %v)", id, state.StatErr))
			continue
		}
		if state.Live {
			offenders = append(offenders, fmt.Sprintf("%s (pid %d)", id, state.PID))
		}
	}
	return offenders, nil
}

func fileStemMatches(name, prefix, suffix string) bool {
	return len(name) > len(prefix)+len(suffix) &&
		name[:len(prefix)] == prefix &&
		name[len(name)-len(suffix):] == suffix
}

func trimAffixes(name, prefix, suffix string) string {
	return name[len(prefix) : len(name)-len(suffix)]
}

// compactHint prints the one-line pointer to `rush sessions compact` when
// a gc/purge run left enough free pages behind to be worth rebuilding for.
// Called from gc/purge only when they actually deleted something and are
// not in --dry-run/--json mode. Thresholds are package vars so tests can
// exercise the hint on multi-MB databases.
var (
	compactHintMinBytes    = int64(64 << 20)
	compactHintMinFraction = 0.5
)

func compactHint(ctx context.Context, conn *sql.DB) {
	stats, err := db.Stats(ctx, conn)
	if err != nil {
		return
	}
	free := stats.FreeBytes
	if free < compactHintMinBytes {
		return
	}
	if float64(free) < compactHintMinFraction*float64(stats.Bytes) {
		return
	}
	fmt.Fprintf(os.Stderr, "rush.db: %s of %s is free pages; run 'rush sessions compact' to reclaim\n",
		humanBytes(free), humanBytes(stats.Bytes))
}

func humanBytes(v int64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(v)/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(v)/(1<<20))
	case v >= 1024:
		return fmt.Sprintf("%.1f KB", float64(v)/1024)
	default:
		return fmt.Sprintf("%d B", v)
	}
}

func percentOf(part, total int64) int {
	if total == 0 {
		return 0
	}
	return int(100 * part / total)
}

func init() {
	sessionsCompactCmd.Flags().Bool("dry-run", false, "Print size, freelist, estimate and gate verdicts without compacting")
	sessionsCompactCmd.Flags().Bool("force", false, "Compact despite live rush hosts or live session locks (their writers may fail with SQLITE_BUSY); never bypasses migrate.lock or a busy WAL checkpoint")
	sessionsCompactCmd.Flags().Bool("json", false, "Emit a single JSON summary object instead of text")
}
