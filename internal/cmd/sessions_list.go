package cmd

// The `sessions list` subcommand: table / NDJSON listing of top-level
// sessions, plus the STATUS-column machinery that classifies each session
// as running / crashed / done / delegating from the locks directory and
// the shared call-tree activity signal.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all top-level sessions",
	Long: `List all top-level (non-child) sessions in this workspace.

Without --json the output is a fixed-width table; with --json each line is
one JSON object suitable for jq / streaming consumers.`,
	Example: `
# Human-readable table
crush sessions list

# Machine-readable (one object per line)
crush sessions list --json | jq 'select(.message_count > 0)'
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sessions, err := a.Sessions.List(cmd.Context())
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		// Filter out internal child sessions (sub-agents, title-generators).
		visible := sessions[:0]
		for _, s := range sessions {
			if s.ParentSessionID != "" {
				continue
			}
			visible = append(visible, s)
		}
		sessions = visible

		// Fork patch (orchestrator UX, round 2 #1): compute STATUS by
		// reading the locks directory once. running = lock exists and
		// holder PID is alive; crashed = lock exists but PID is dead
		// (will be auto-reclaimed on next acquire); blank = no lock,
		// session is at rest. The lock dir read is one syscall + N
		// directory entries; the PID liveness check is the same cheap
		// per-PID probe `sessions reap` uses.
		statusByID := computeSessionStatuses(a)

		// A dead-PID lock can mean two things: a genuine mid-turn crash,
		// or a `crush run` that finished cleanly (last assistant turn
		// ended with end_turn) and exited within the ~60s heartbeat
		// sweep window — its lock file is still on disk but the PID is
		// gone. Reclassify those to "done" so a clean exit isn't shown
		// as "crashed". Cheap: only the handful of sessions with a stale
		// lock actually hit the message store.
		statusByID = reclassifyCrashedAsDone(cmd.Context(), a, sessions, statusByID)

		// Sub-agent awareness: a "running" session that is currently blocked
		// inside an `agent` delegation gets promoted to "delegating" so the
		// STATUS column distinguishes "top-level agent is working" from "top-
		// level agent is waiting on a sub-agent". The freshness signal comes
		// from the shared call-tree walk (sessions_activity.go), NOT from the
		// lock mtime, so it reflects the sub-agent actually making progress.
		statusByID = markDelegatingSessions(cmd.Context(), a, sessions, statusByID)

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, s := range sessions {
				item := makeSessionListItem(s)
				if st := statusByID[s.ID]; st != "" {
					item.Status = st
				}
				if err := enc.Encode(item); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "HASH\tID\tTITLE\tMSGS\tSTATUS\tUPDATED\tTOKENS\tCOST")
		for _, s := range sessions {
			fmt.Fprintf(
				tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t$%.4f\n",
				short(session.HashID(s.ID)),
				s.ID,
				truncate(s.Title, 40),
				s.MessageCount,
				statusOrDash(statusByID[s.ID]),
				time.Unix(s.UpdatedAt, 0).Format("2006-01-02 15:04"),
				s.PromptTokens+s.CompletionTokens,
				s.Cost,
			)
		}
		return tw.Flush()
	},
}

// computeSessionStatuses returns sessionID → status ("running" | "crashed").
// Sessions not in the map are at rest (no lock). Cheap: one directory read +
// one PID probe per lock file.
//
// A session counts as "running" if EITHER the PID it recorded is alive OR
// its heartbeat (lock file mtime) is still fresh. The PID check alone is
// not reliable on Windows: tryLockFile takes a mandatory, whole-file
// LockFileEx lock for the holder's entire lifetime, so a plain read of the
// PID from another process fails for as long as the session is alive (see
// the Windows note on session.readLockFile) — without the heartbeat
// fallback, every live session on Windows would misreport as "crashed".
//
// Takes the already-booted *app.App (rather than re-deriving the data
// directory from --cwd) so it honors --data-dir / a configured
// data_directory the same way `sessions locks` does — see task #233,
// the same cwd-hardcoding bug class as task #219/#224/#231.
//
// Deliberately does NOT delegate to session.InspectSessionLock (unlike
// sessions_watch.go's isSessionFinished, task #241): InspectSessionLock's
// shape is "mtime fresh is the fast path; only when mtime already looks
// stale, fall back to probing the PID". This function's shape is the
// opposite — a CONFIRMED pid > 0 is trusted unconditionally, mtime is only
// the fallback for an unreadable/ambiguous PID (pid <= 0, the Windows norm
// for a live session) — so the two are not interchangeable without
// changing behavior here. The pid > 0 branch below is bounded by
// session.MaxPidFallbackAge for the same reason InspectSessionLock is
// (task #235/#241): without a bound, a lock abandoned by a killed/crashed
// `crush run` whose recorded PID the OS later recycles for an unrelated,
// currently-running process would report "running" forever, with
// `sessions list`'s STATUS column never self-healing to "crashed"/"done".
func computeSessionStatuses(a *app.App) map[string]string {
	if a == nil {
		return nil
	}
	locksDir := filepath.Join(a.Config().Options.DataDirectory, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".lock") {
			continue
		}
		sessionID := strings.TrimSuffix(strings.TrimPrefix(name, "session-"), ".lock")
		path := filepath.Join(locksDir, name)
		pid := session.ReadLockPID(path)
		info, statErr := entry.Info()
		// A CONFIRMED-dead PID (pid > 0 but not alive) is trustworthy on
		// its own. pid <= 0 is ambiguous — "unreadable", not necessarily
		// dead (see the Windows note on session.readLockFile) — so only
		// then do we fall back to heartbeat freshness. A CONFIRMED-alive
		// PID is trusted only within session.MaxPidFallbackAge of the
		// lock's mtime — past that, the lock is old enough that no
		// genuinely healthy holder could still be running it, so a
		// currently-alive PID is far more likely to be an OS PID-reuse
		// coincidence than the original holder (task #241).
		var alive bool
		switch {
		case pid > 0 && statErr == nil && time.Since(info.ModTime()) >= session.MaxPidFallbackAge:
			alive = false
		case pid > 0:
			alive = session.IsProcessAlive(pid)
		case statErr == nil:
			alive = time.Since(info.ModTime()) <= session.LockStaleDuration
		}
		if alive {
			out[sessionID] = "running"
		} else {
			out[sessionID] = "crashed"
		}
	}
	return out
}

// reclassifyCrashedAsDone promotes a "crashed" status to "done" when the
// session's last ASSISTANT message finished cleanly (FinishReasonEndTurn).
// A dead-PID lock without such a clean finish stays "crashed" — that's the
// genuine mid-turn-crash case. Mutates and returns statusByID in place so
// both the JSON and table render paths share the same corrected map.
//
// Only sessions currently flagged "crashed" hit the message store, so the
// cost is proportional to the number of stale locks (usually zero or one).
func reclassifyCrashedAsDone(
	ctx context.Context,
	a *app.App,
	sessions []session.Session,
	statusByID map[string]string,
) map[string]string {
	if statusByID == nil || a == nil {
		return statusByID
	}
	for _, s := range sessions {
		if statusByID[s.ID] != "crashed" {
			continue
		}
		msgs, err := a.Messages.List(ctx, s.ID)
		if err != nil {
			continue
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant {
				continue
			}
			if msgs[i].FinishReason() == message.FinishReasonEndTurn {
				statusByID[s.ID] = "done"
			}
			break
		}
	}
	return statusByID
}

// markDelegatingSessions promotes a "running" status to "delegating" when
// the session's freshest activity is coming from an in-flight sub-agent
// delegation rather than the top-level agent itself. This is the STATUS-
// column consumer of the shared call-tree activity signal: it lets an
// operator scanning `sessions list` see at a glance which running sessions
// are currently blocked on (and being kept alive by) a sub-agent.
//
// Only sessions already flagged "running" are probed — at-rest / crashed /
// done sessions are left untouched. All of them are checked in ONE batched
// SQL query (computeCallTreeActivityBatch) instead of one call-tree query
// per running session, so `sessions list` stays O(1) queries for this step
// regardless of how many sessions happen to be running concurrently.
func markDelegatingSessions(
	ctx context.Context,
	a *app.App,
	sessions []session.Session,
	statusByID map[string]string,
) map[string]string {
	if statusByID == nil || a == nil {
		return statusByID
	}

	running := make([]session.Session, 0, len(sessions))
	for _, s := range sessions {
		if statusByID[s.ID] == "running" {
			running = append(running, s)
		}
	}
	if len(running) == 0 {
		return statusByID
	}

	ids := make([]string, len(running))
	for i, s := range running {
		ids[i] = s.ID
	}
	activity := computeCallTreeActivityBatch(ctx, a, ids)

	for _, s := range running {
		act, ok := activity[s.ID]
		if !ok {
			continue
		}
		// Baseline = the session's own updated_at. A descendant sub-agent
		// message newer than that means the live edge of work is inside a
		// delegation. (The session row's updated_at is NOT bumped by child
		// message inserts — see the DB triggers — so this comparison is
		// meaningful.)
		if act.LatestUnix > s.UpdatedAt && act.SubAgentActive {
			statusByID[s.ID] = "delegating"
		}
	}
	return statusByID
}

func statusOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sessionListItem is the JSON shape of `crush sessions list --json`. Held
// as a separate struct (rather than just marshalling session.Session
// directly) so the wire-stable field names don't drift if session.Session
// gains internal fields we don't want to publish.
type sessionListItem struct {
	ID           string  `json:"id"`
	Hash         string  `json:"hash"`
	Title        string  `json:"title"`
	MessageCount int64   `json:"message_count"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
	Tokens       int64   `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	YoloEnabled  bool    `json:"yolo_enabled"`
	// Status is "running" (lock exists, holder PID alive), "crashed"
	// (lock exists but PID dead — will be auto-reclaimed) or "" (at rest).
	// Computed live from the locks directory at list time. omitempty so
	// the field is absent for at-rest sessions, keeping the wire shape
	// minimal for the common case.
	Status string `json:"status,omitempty"`
}

// makeSessionListItem projects a session.Session into the wire-stable
// sessionListItem shape used by `crush sessions list --json`.
func makeSessionListItem(s session.Session) sessionListItem {
	return sessionListItem{
		ID:           s.ID,
		Hash:         session.HashID(s.ID),
		Title:        s.Title,
		MessageCount: s.MessageCount,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		Tokens:       s.PromptTokens + s.CompletionTokens,
		CostUSD:      s.Cost,
		YoloEnabled:  s.YoloEnabled,
	}
}
