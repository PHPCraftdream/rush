package cmd

// Batch mutation/guard side of the four session action commands (R12-B part
// 2): with RUSH_BATCH_RUNTIME on, `sessions reset` wipes history through the
// R10c BatchResetOwnerHistory (refusing while any root is nonterminal),
// `sessions gc` refuses to collect sessions that own live or unknown batch
// work, `sessions compact` refuses while batch work is live, and `sessions
// kill` annotates its report so a held batch root is never read as "nothing
// running". Flag off, every command keeps its legacy path byte-for-byte.
// Guards are conservative (R10a rule): an incomplete read counts as live
// work, never as absence.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchSessionGuardReason returns "" when the session's batch facts are
// safely at rest, or a human-readable reason naming the offending root:
// an incomplete read first (conservative), then a held root, then the
// pending-inclusive delivery debt (terminal roots included).
func batchSessionGuardReason(f batchSessionFacts) string {
	if f.Unknown {
		return "batch read was incomplete; live batch work is unknown"
	}
	for _, r := range f.Roots {
		if r.Held {
			verdict := r.State
			if r.AwaitingTermination {
				verdict = r.State + " (stopped but awaiting termination)"
			}
			return fmt.Sprintf("batch root %s (tool call %s) is %s", short(r.RootID), r.ToolCallID, verdict)
		}
	}
	if f.GuardDebt {
		return fmt.Sprintf("batch delivery debt of %d unreacted notice(s) (terminal roots included)", f.GuardDebtN)
	}
	return ""
}

// batchResetRefusedError turns a *session.BatchResetRefusal into the same
// output shape the legacy reset refusal uses, naming the offending root and
// surfacing the cancelling = "stopped but awaiting termination" case.
func batchResetRefusedError(sessionID string, refusal *session.BatchResetRefusal) error {
	state := refusal.State
	if state == "cancelling" {
		state = "cancelling (stopped but awaiting termination)"
	}
	return fmt.Errorf("session %s is still being worked on (batch root %s tool call %s is %s); "+
		"nothing was reset. Stop it first (`rush sessions cancel %s` / `rush batch stop`), then reset again",
		sessionID, refusal.RootID, refusal.ToolCallID, state, sessionID)
}

// resetActivityRefusal is the legacy reset path's live-work gate: the legacy
// classifier decides "still being worked on" (driver, running jobs,
// delegations); --force skips only the lock half, which the caller handled.
// The flag-ON batch path does NOT use it: the R10c batch reset refuses
// atomically on any nonterminal root, which is its live-work gate.
func resetActivityRefusal(ctx context.Context, a *app.App, sessionID string, force bool) error {
	act, actErr := a.SessionActivity(ctx, sessionID)
	if actErr != nil {
		return resetRefusedError(sessionID, "live-work state could not be read (fail closed)")
	}
	if f := act.Facts; force {
		if f.Driver != nil || f.OwnRunningJobs > 0 || f.LiveDelegations > 0 || len(f.OpenSchedules) > 0 || len(f.Unreadable) > 0 {
			return resetRefusedError(sessionID, act.Verdict.Description)
		}
	} else if kindIsLive(act.Verdict.Kind) {
		return resetRefusedError(sessionID, act.Verdict.Description)
	}
	return nil
}

// batchResetSessionHistory is the flag-ON reset path: the R10c batch reset
// (check + delete in ONE writer transaction; refusal writes nothing). Its
// atomic refusal of any nonterminal batch root IS the live-work gate -- no
// legacy classifier or ledger recovery runs here. The plain-wipe fallback
// exists only for a configless App (nil DB).
func batchResetSessionHistory(ctx context.Context, a *app.App, sessionID string) (session.BatchResetOutcome, error) {
	if a.DB() == nil {
		// No database can hold batch rows: the plain wipe is all there is.
		if err := a.Messages.DeleteSessionMessages(ctx, sessionID); err != nil {
			return session.BatchResetOutcome{}, fmt.Errorf("failed to reset session %s: %w", sessionID, err)
		}
		return session.BatchResetOutcome{}, nil
	}
	out, err := session.NewBatchStore(a.DB()).BatchResetOwnerHistory(ctx, a.Messages, sessionID)
	var refusal *session.BatchResetRefusal
	if errors.As(err, &refusal) {
		return session.BatchResetOutcome{}, batchResetRefusedError(sessionID, refusal)
	}
	if errors.Is(err, session.ErrBatchResetRefused) {
		return session.BatchResetOutcome{}, resetRefusedError(sessionID, err.Error())
	}
	if err != nil {
		return session.BatchResetOutcome{}, fmt.Errorf("failed to reset session %s: %w", sessionID, err)
	}
	return out, nil
}

// gcDropBatchProtected removes sessions whose batch facts show live or
// unknown work (or unreacted debt) from the gc candidate list, reporting
// each keep to stderr. Never deletes on an incomplete read.
func gcDropBatchProtected(a *app.App, candidates []gcItem) []gcItem {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	facts := batchRuntimeFacts(context.Background(), a, a.Config().Options.DataDirectory, ids)
	if facts == nil {
		return candidates
	}
	kept := make([]gcItem, 0, len(candidates))
	for _, c := range candidates {
		if reason := batchSessionGuardReason(facts[c.ID]); reason != "" {
			fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", short(session.HashID(c.ID)), truncate(c.Title, 40), reason)
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// gcBatchRetention runs the batch-side numbers of --jobs-older-than through
// the R10c retention API at the caller-chosen age instead of duplicating its
// cutoff policy. Returns the expired root/node count; verb is "would purge"
// on a dry run.
func gcBatchRetention(ctx context.Context, a *app.App, dryRun bool, age time.Duration) (int64, error) {
	bs := session.NewBatchStore(a.DB())
	if dryRun {
		return bs.BatchCountJobsOlderThan(ctx, age)
	}
	return bs.BatchPurgeJobsOlderThan(ctx, age)
}

// compactBatchLiveSessions lists the sessions whose batch facts show live or
// unknown work (or debt), as "id (reason)" offender strings. Nil, nil when
// the flag is off.
func compactBatchLiveSessions(ctx context.Context, a *app.App, dataDir string) ([]string, error) {
	if !config.BatchRuntimeEnabled() {
		return nil, nil
	}
	sessions, err := a.Sessions.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("batch gate: list sessions: %w", err)
	}
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	facts := batchRuntimeFacts(ctx, a, dataDir, ids)
	if facts == nil {
		return nil, nil
	}
	var offenders []string
	for _, s := range sessions {
		if reason := batchSessionGuardReason(facts[s.ID]); reason != "" {
			offenders = append(offenders, fmt.Sprintf("%s (%s)", short(session.HashID(s.ID)), reason))
		}
	}
	return offenders, nil
}

// batchKillLiveNote is the `sessions kill` guard: with the flag on, a session
// whose batch roots are held (or whose batch read was incomplete) must never
// be reported as at rest just because its lock file is free -- batch nodes
// are not process-lock holders. Returns "" when off, at rest, or unreadable
// (the read itself reports its own conservative note).
func batchKillLiveNote(ctx context.Context, dataDir, sessionID string) string {
	if !config.BatchRuntimeEnabled() {
		return ""
	}
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return fmt.Sprintf("note: batch work for session %s could not be read (%v); treat live batch work as unknown", sessionID, err)
	}
	defer func() { _ = db.ReleaseConn(conn) }()
	reader := session.NewBatchLiveReader(db.New(conn), dataDir)
	facts := reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(conn), []string{sessionID})
	f := facts[sessionID]
	if reason := batchSessionGuardReason(f); reason != "" {
		return fmt.Sprintf("note: batch work for session %s is not stopped by killing the lock holder: %s "+
			"(inspect with `rush sessions jobs %s`; stop it with `rush batch stop`)", sessionID, reason, sessionID)
	}
	return ""
}
