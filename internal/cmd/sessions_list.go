package cmd

// The `sessions list` subcommand: table / NDJSON listing of top-level
// sessions. The STATUS column is the one session-activity classifier's
// verdict (App.SessionActivityBatch), the same verdicts `sessions why`
// prints -- there is no second status machinery here.

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

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
rush sessions list

# Machine-readable (one object per line)
rush sessions list --json | jq 'select(.message_count > 0)'
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

		// One classifier for the whole list (R-ACT): every STATUS comes from
		// App.SessionActivityBatch -- the same verdicts `sessions why` prints --
		// with no per-session promotion layers. The Kind -> STATUS mapping:
		// in turn / between turns -> "running" (live work), delegating ->
		// "delegating", crashed -> "crashed", ended -> "done", idle -> blank
		// (at rest). In-process knowledge (the coordinator's parked-delegation
		// registry) is deliberately not consulted: a parked delegation keeps
		// its own async_jobs row live, so another process sees the same
		// verdict (decision 9).
		activities, err := a.SessionActivityBatch(cmd.Context(), idsOf(sessions))
		if err != nil {
			return fmt.Errorf("failed to classify sessions: %w", err)
		}
		statusByID := make(map[string]string, len(sessions))
		for id, act := range activities.ByID {
			statusByID[id] = listStatus(act.Verdict)
		}

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

// idsOf projects the session list to the id slice the batched
// classifier takes.
func idsOf(sessions []session.Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

// listStatus maps the classifier's verdict to the STATUS-column vocabulary
// ("running" / "delegating" / "crashed" / "done"; "" = at rest). One mapping
// for every cmd consumer, so the commands cannot drift apart.
func listStatus(v session.ActivityVerdict) string {
	switch v.Kind {
	case session.ActivityInTurn, session.ActivityBetweenTurns:
		return "running"
	case session.ActivityDelegating:
		return "delegating"
	case session.ActivityCrashed:
		return "crashed"
	case session.ActivityEnded:
		return "done"
	}
	return ""
}

func statusOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sessionListItem is the JSON shape of `rush sessions list --json`. Held
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
	// EndedReason is how the session's last run ended (its exit_reason);
	// empty while a run is in progress or when none ever ended.
	EndedReason string `json:"ended_reason,omitempty"`
	// Status is "running" (live work: in a turn or between turns),
	// "delegating" (a live sub-agent delegation), "crashed" (a crash fact,
	// no live work), "done" (a recorded ended_reason) or absent (at rest).
	// Classified by the one session-activity classifier; omitempty keeps
	// the wire shape minimal for at-rest sessions.
	Status string `json:"status,omitempty"`
}

// makeSessionListItem projects a session.Session into the wire-stable
// sessionListItem shape used by `rush sessions list --json`.
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
		EndedReason:  s.EndedReason,
	}
}
