package cmd

// Batch mutation/guard side of the four session action commands (R12-B part
// 2): `sessions reset` wipes history through the
// R10c BatchResetOwnerHistory (refusing while any root is nonterminal),
// `sessions gc` refuses to collect sessions that own live or unknown batch
// work, `sessions compact` refuses while batch work is live, and `sessions
// kill` annotates its report so a held batch root is never read as "nothing
// running".
// Guards are conservative (R10a rule): an incomplete read counts as live
// work, never as absence.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	sesslocks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/locks"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
	return sesskit.BatchResetRefusedError(sessionID, refusal)
}

// resetLiveLoopRefusal is the reset's live-loop gate: the batch
// reset's atomic refusal covers batch work, but a live `rush run` loop
// between turns holds no batch root and no lock, so `reset --force` would
// otherwise wipe a session a loop still drives (the R8A-3 bug). Refuses
// under --force too.
func resetLiveLoopRefusal(ctx context.Context, a *app.App, sessionID string) error {
	return sesskit.ResetLiveLoopRefusal(ctx, a, sessionID)
}

// batchResetSessionHistory is the reset path: the R10c batch reset
// (check + delete in ONE writer transaction; refusal writes nothing). Its
// atomic refusal of any nonterminal batch root IS the live-work gate -- the
// only predecessor step is H4 W2's best-effort owner-scope dead-host
// recovery (BatchRecoverOwnerScope), which never touches the legacy
// ledger. The plain-wipe fallback
// exists only for a configless App (nil DB).
func batchResetSessionHistory(ctx context.Context, a *app.App, sessionID string) (session.BatchResetOutcome, error) {
	return sesskit.BatchResetSessionHistory(ctx, a, sessionID)
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

// gcDropServiceBatchOwners keeps every API service session that still owns
// batch rows, finished ones included. A service session never has messages, and
// its rows cascade with it, so collecting it as an old empty session would erase
// the history of finished roots before the batch retention purge is due. Once
// retention removed the rows the next gc collects it like any empty session.
// Ordinary sessions keep the debt-based protection of gcDropBatchProtected.
func gcDropServiceBatchOwners(ctx context.Context, a *app.App, candidates []gcItem, all []session.Session) []gcItem {
	service := make(map[string]bool)
	for _, item := range all {
		if item.Origin == message.OriginAPIService {
			service[item.ID] = true
		}
	}
	store := session.NewBatchStore(a.DB())
	kept := make([]gcItem, 0, len(candidates))
	for _, c := range candidates {
		if service[c.ID] {
			owns, err := store.BatchOwnsRows(ctx, c.ID)
			if err != nil || owns {
				reason := "it is an API service session that owns batch rows"
				if err != nil {
					reason = "its batch rows could not be read"
				}
				fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", short(session.HashID(c.ID)), truncate(c.Title, 40), reason)
				continue
			}
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
// unknown work (or debt), as "id (reason)" offender strings.
func compactBatchLiveSessions(ctx context.Context, a *app.App, dataDir string) ([]string, error) {
	sessions, err := a.Sessions.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("batch gate: list sessions: %w", err)
	}
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	facts := batchRuntimeFacts(ctx, a, dataDir, ids)
	var offenders []string
	for _, s := range sessions {
		if reason := batchSessionGuardReason(facts[s.ID]); reason != "" {
			offenders = append(offenders, fmt.Sprintf("%s (%s)", short(session.HashID(s.ID)), reason))
		}
	}
	return offenders, nil
}

// batchKillLiveNote разделяет физически живой/неизвестный scope и долг доставки.
func batchKillLiveNote(ctx context.Context, dataDir, sessionID string) sesslocks.BatchKillScopeState {
	if _, err := os.Stat(storepath.StoreV1Path(dataDir)); err != nil {
		if os.IsNotExist(err) {
			return sesslocks.BatchKillScopeState{} // Rescue не создаёт отсутствующую БД ради чтения.
		}
		return sesslocks.BatchKillScopeState{Note: fmt.Sprintf("batch store is unreadable for session %s: %v", sessionID, err), Unconfirmed: true}
	}
	conn, err := db.Connect(ctx, dataDir)
	if err != nil {
		return sesslocks.BatchKillScopeState{Note: fmt.Sprintf("note: batch work for session %s could not be read (%v); treat live batch work as unknown", sessionID, err), Unconfirmed: true}
	}
	defer func() { _ = db.ReleaseConn(conn) }()
	reader := session.NewBatchLiveReader(db.New(conn), dataDir)
	facts := reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(conn), db.New(conn), []string{sessionID})
	f := facts[sessionID]
	state := sesslocks.BatchKillScopeState{Unconfirmed: f.Unknown}
	for _, root := range f.Roots {
		state.Unconfirmed = state.Unconfirmed || root.Held
	}
	if reason := batchSessionGuardReason(f); reason != "" {
		state.Note = fmt.Sprintf("note: batch work for session %s is not stopped by killing the lock holder: %s "+
			"(inspect with `rush sessions jobs %s`; stop it with `rush batch stop`)", sessionID, reason, sessionID)
	}
	return state
}
