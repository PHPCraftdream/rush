package sessbatch

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
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	sesslocks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live/locks"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// batchSessionGuardReason returns "" when the session's batch facts are
// safely at rest, or a human-readable reason naming the offending root:
// an incomplete read first (conservative), then a held root, then the
// pending-inclusive delivery debt (terminal roots included).
func batchSessionGuardReason(f sesskit.BatchSessionFacts) string {
	if f.Unknown {
		return "batch read was incomplete; live batch work is unknown"
	}
	for _, r := range f.Roots {
		if r.Held {
			verdict := r.State
			if r.AwaitingTermination {
				verdict = r.State + " (stopped but awaiting termination)"
			}
			return fmt.Sprintf("batch root %s (tool call %s) is %s", cliout.Short(r.RootID), r.ToolCallID, verdict)
		}
	}
	if f.GuardDebt {
		return fmt.Sprintf("batch delivery debt of %d unreacted notice(s) (terminal roots included)", f.GuardDebtN)
	}
	return ""
}

// GcDropBatchProtected removes sessions whose batch facts show live or
// unknown work (or unreacted debt) from the gc candidate list, reporting
// each keep to stderr. Never deletes on an incomplete read.
func GcDropBatchProtected(a *app.App, candidates []GcItem) []GcItem {
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.ID
	}
	facts := batchRuntimeFacts(context.Background(), a, a.Config().Options.DataDirectory, ids)
	kept := make([]GcItem, 0, len(candidates))
	for _, c := range candidates {
		if reason := batchSessionGuardReason(facts[c.ID]); reason != "" {
			fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", cliout.Short(coresession.HashID(c.ID)), cliout.Truncate(c.Title, 40), reason)
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// GcDropServiceBatchOwners keeps every API service session that still owns
// batch rows, finished ones included. A service session never has messages, and
// its rows cascade with it, so collecting it as an old empty session would erase
// the history of finished roots before the batch retention purge is due. Once
// retention removed the rows the next gc collects it like any empty session.
// Ordinary sessions keep the debt-based protection of GcDropBatchProtected.
func GcDropServiceBatchOwners(ctx context.Context, a *app.App, candidates []GcItem, all []coresession.Session) []GcItem {
	service := make(map[string]bool)
	for _, item := range all {
		if item.Origin == message.OriginAPIService {
			service[item.ID] = true
		}
	}
	store := session.NewBatchStore(a.DB())
	kept := make([]GcItem, 0, len(candidates))
	for _, c := range candidates {
		if service[c.ID] {
			owns, err := store.BatchOwnsRows(ctx, c.ID)
			if err != nil || owns {
				reason := "it is an API service session that owns batch rows"
				if err != nil {
					reason = "its batch rows could not be read"
				}
				fmt.Fprintf(os.Stderr, "kept session %s (%s): %s\n", cliout.Short(coresession.HashID(c.ID)), cliout.Truncate(c.Title, 40), reason)
				continue
			}
		}
		kept = append(kept, c)
	}
	return kept
}

// GcBatchRetention runs the batch-side numbers of --jobs-older-than through
// the R10c retention API at the caller-chosen age instead of duplicating its
// cutoff policy. Returns the expired root/node count; verb is "would purge"
// on a dry run.
func GcBatchRetention(ctx context.Context, a *app.App, dryRun bool, age time.Duration) (int64, error) {
	bs := session.NewBatchStore(a.DB())
	if dryRun {
		return bs.BatchCountJobsOlderThan(ctx, age)
	}
	return bs.BatchPurgeJobsOlderThan(ctx, age)
}

// CompactBatchLiveSessions lists the sessions whose batch facts show live or
// unknown work (or debt), as "id (reason)" offender strings.
func CompactBatchLiveSessions(ctx context.Context, a *app.App, dataDir string) ([]string, error) {
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
			offenders = append(offenders, fmt.Sprintf("%s (%s)", cliout.Short(coresession.HashID(s.ID)), reason))
		}
	}
	return offenders, nil
}

// BatchKillLiveNote разделяет физически живой/неизвестный scope и долг доставки.
func BatchKillLiveNote(ctx context.Context, dataDir, sessionID string) sesslocks.BatchKillScopeState {
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

// GcItem is one gc candidate: identity, title, age and the classification
// reason it was collected under.
type GcItem struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	AgeHours float64 `json:"age_hours"`
	Reason   string  `json:"reason"`
}
