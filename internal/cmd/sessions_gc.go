package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsGcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect stale sessions",
	Long: `Delete sessions that are no longer useful:

  1. Sessions older than --older-than (default 7 days) with zero messages.
  2. Sessions with ID prefix "ping-" older than 1 hour.
  3. Child sessions (parent_id != "") whose parent no longer exists,
     older than 24h.

With --jobs-older-than, ALSO purges terminal (not 'running'), delivered-or-
voided rows from the phase-4 durable async job ledger (async_jobs and
session_notices) older than the given age. A 'running' row, an undelivered
('pending') row and delivered-but-unreacted debt (a notice whose reaction turn
is still owed) are NEVER purged regardless of age, nor is a delegation row whose
child session still has running work or unreacted debt -- this only removes
rows whose async command/delegation already finished (or was voided by a
Rerun), whose notice was delivered and owes no reaction, and stayed in that
state past the age.
Without --jobs-older-than, job retention runs only with its own
fixed 7-day window, and only where a rush process is running: the web
server purges every 60s (together with its dead-host sweep), and each
"rush run" loop purges when it starts and again every 60s while it runs (the
same pass). Nothing else purges in the background, so rows of a workspace
used through neither wait for one of those. This flag is a separate, opt-in, one-shot trigger with its own age,
not a replacement for either.

Use --dry-run to print what would be deleted (or purged) without deleting.
Use --max-sessions to cap the number of SESSION deletions per run (does not
bound --jobs-older-than's purge).
Use --json to emit one JSON object per deleted (or would-be-deleted) session;
with --jobs-older-than a first line {"kind":"async_jobs","dry_run":...,
"async_jobs":N,"session_notices":M} reports the purged (or would-be-purged)
row counts.`,
	Example: `
# Dry run: show what would be collected
rush sessions gc --dry-run

# Collect with defaults (7 days, no limit)
rush sessions gc

# Collect sessions older than 3 days, max 50 deletions
rush sessions gc --older-than 3d --max-sessions 50

# Also purge terminal async job/notice rows older than 3 days
rush sessions gc --jobs-older-than 3d

# Machine-readable output
rush sessions gc --dry-run --json
  `,
	RunE: sessionsGcCmdRun,
}

type gcItem struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	AgeHours float64 `json:"age_hours"`
	Reason   string  `json:"reason"`
}

// gcJobsSummary is the --json line for --jobs-older-than: how many terminal
// async_jobs and session_notices rows were purged (or, with --dry-run, would
// be).
type gcJobsSummary struct {
	Kind           string `json:"kind"`
	DryRun         bool   `json:"dry_run"`
	OlderThan      string `json:"older_than"`
	AsyncJobs      int64  `json:"async_jobs"`
	SessionNotices int64  `json:"session_notices"`
}

func sessionsGcCmdRun(cmd *cobra.Command, args []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	olderThanStr, _ := cmd.Flags().GetString("older-than")
	jobsOlderThanStr, _ := cmd.Flags().GetString("jobs-older-than")
	maxSessions, _ := cmd.Flags().GetInt("max-sessions")
	asJSON, _ := cmd.Flags().GetBool("json")

	olderThan, err := parseDurationDays(olderThanStr)
	if err != nil {
		return fmt.Errorf("--older-than: %w", err)
	}
	// "" means the flag was never passed: job retention stays entirely on
	// the fixed 7-day window run by the 60s pass of the web server and of each
	// running `rush run` loop, and at its start (doc sec.3.7) -- this step is
	// opt-in, not a second default-7d trigger every plain `sessions gc`
	// invocation would also pay for.
	var (
		jobsOlderThan time.Duration
		purgeJobs     bool
	)
	if jobsOlderThanStr != "" {
		jobsOlderThan, err = parseDurationDays(jobsOlderThanStr)
		if err != nil {
			return fmt.Errorf("--jobs-older-than: %w", err)
		}
		// A5: an age <= 0 makes the retention cutoff "now or later", purging
		// terminal rows regardless of how recent -- including unreacted debt
		// that just hasn't been read yet. Reject outright rather than letting
		// an operator typo (or an intentional "purge everything now") nuke
		// live obligations.
		if jobsOlderThan <= 0 {
			return fmt.Errorf("--jobs-older-than: must be a positive duration, got %q", jobsOlderThanStr)
		}
		purgeJobs = true
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessions, err := a.Sessions.ListAll(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Build a set of existing session IDs for orphan detection.
	existingIDs := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		existingIDs[s.ID] = struct{}{}
	}

	now := time.Now()
	var toDelete []gcItem

	for _, s := range sessions {
		age := now.Sub(time.Unix(s.CreatedAt, 0))
		reason := classifyForGC(s, age, existingIDs, olderThan)
		if reason == "" {
			continue
		}
		toDelete = append(toDelete, gcItem{
			ID:       s.ID,
			Title:    s.Title,
			AgeHours: age.Hours(),
			Reason:   reason,
		})
	}

	// Cap deletions if --max-sessions is set.
	if maxSessions > 0 && len(toDelete) > maxSessions {
		toDelete = toDelete[:maxSessions]
	}

	// --jobs-older-than: independent of the session-collection rules above
	// and of --max-sessions (which only bounds SESSION deletions) -- runs
	// regardless of whether any session was collected, so `sessions gc
	// --jobs-older-than` with nothing to collect on the session side still
	// performs job retention.
	var jobsAffected, noticesAffected int64
	if purgeJobs {
		store := a.AsyncJobStore()
		if store == nil {
			return fmt.Errorf("--jobs-older-than: no async job store available (SkipAgentSetup or no data dir)")
		}
		if dryRun {
			jobsAffected, noticesAffected, err = store.CountJobsOlderThan(cmd.Context(), jobsOlderThan)
		} else {
			jobsAffected, noticesAffected, err = store.PurgeJobsOlderThan(cmd.Context(), jobsOlderThan)
		}
		if err != nil {
			return fmt.Errorf("--jobs-older-than: %w", err)
		}
		if asJSON {
			// C19: --json used to report nothing for the job purge. One summary
			// line (no "id"; "kind":"async_jobs") precedes the per-session lines.
			if err := json.NewEncoder(os.Stdout).Encode(gcJobsSummary{
				Kind: "async_jobs", DryRun: dryRun, OlderThan: jobsOlderThanStr,
				AsyncJobs: jobsAffected, SessionNotices: noticesAffected,
			}); err != nil {
				return err
			}
		}
		if !asJSON && (jobsAffected > 0 || noticesAffected > 0) {
			verb := "would purge"
			if !dryRun {
				verb = "purged"
			}
			fmt.Fprintf(os.Stderr, "%s %d async job row(s) and %d notice row(s) older than %s\n",
				verb, jobsAffected, noticesAffected, jobsOlderThanStr)
		}
	}

	if len(toDelete) == 0 {
		if !asJSON && jobsAffected == 0 && noticesAffected == 0 {
			fmt.Println("(nothing to collect)")
		}
		return nil
	}

	// Rule 1's classification is exactly MessageCount == 0 (classifyForGC)
	// and deliberately does NOT consider message roles: message_count is a
	// plain row count maintained by unconditional insert/delete triggers,
	// so a session holding only system messages has MessageCount > 0 and
	// is kept.

	for _, item := range toDelete {
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			if err := enc.Encode(item); err != nil {
				return err
			}
		} else {
			action := "would delete"
			if !dryRun {
				action = "deleted"
			}
			fmt.Fprintf(os.Stderr, "%s session %s (%s): %s\n", action, short(session.HashID(item.ID)), truncate(item.Title, 40), item.Reason)
		}

		if !dryRun {
			if err := a.Sessions.Delete(cmd.Context(), item.ID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to delete session %s: %v\n", item.ID, err)
			}
		}
	}

	if !asJSON {
		prefix := "would delete"
		if !dryRun {
			prefix = "deleted"
		}
		fmt.Fprintf(os.Stderr, "%s %d session(s)\n", prefix, len(toDelete))
	}

	return nil
}

// classifyForGC returns a human-readable reason if the session should be
// garbage-collected, or "" if it should be kept.
func classifyForGC(s session.Session, age time.Duration, existingIDs map[string]struct{}, olderThan time.Duration) string {
	// Rule 2: ping- sessions older than 1 hour.
	if strings.HasPrefix(s.ID, "ping-") && age > 1*time.Hour {
		return "ping session older than 1 hour"
	}

	// Rule 3: orphaned child sessions older than 24h.
	if s.ParentSessionID != "" {
		if _, ok := existingIDs[s.ParentSessionID]; !ok && age > 24*time.Hour {
			return "orphaned child session (parent deleted)"
		}
	}

	// Rule 1: old sessions with zero messages.
	if age > olderThan && s.MessageCount == 0 {
		return "empty session older than threshold"
	}

	return ""
}

// parseDurationDays parses a duration string like "7d", "3d", "24h", or
// standard Go duration formats.
func parseDurationDays(s string) (time.Duration, error) {
	if s == "" {
		return 7 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		// strconv.Atoi, not fmt.Sscanf: Sscanf parses the leading digits and
		// reports no error for the remainder, so "7dd" trimmed to "7d" parsed
		// as 7 and was silently accepted as a week. Same partial-parse class
		// as the --since bug fixed in parsePlainInt.
		d, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

func init() {
	sessionsGcCmd.Flags().Bool("dry-run", false, "Print what would be deleted without deleting")
	sessionsGcCmd.Flags().String("older-than", "7d", "Delete empty sessions older than this (e.g. 7d, 24h, 30m)")
	sessionsGcCmd.Flags().String("jobs-older-than", "", "Also purge terminal async_jobs/session_notices rows (phase-4 ledger) older than this (e.g. 3d, 12h); empty = only the fixed 7-day retention (web server every 60s, each rush run at start and every 60s while it runs)")
	sessionsGcCmd.Flags().Int("max-sessions", 0, "Maximum number of sessions to delete (0 = unlimited)")
	sessionsGcCmd.Flags().Bool("json", false, "Emit one JSON object per deleted session (plus a job-count summary line with --jobs-older-than)")
}
