package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCostCmd = &cobra.Command{
	Use:   "cost",
	Short: "Show cost breakdown across sessions",
	Long: `Show cost and token usage broken down by model, day, or session.

By default groups by model. Use --by to change the grouping:
  model   — group by the session's model (default)
  day     — group by date (YYYY-MM-DD)
  session — show per-session breakdown (top N by cost)
  total   — just print the grand total

Use --since to filter to sessions updated within the given duration.
Supports Go durations (30m, 24h), day suffix (7d), or plain integers
(interpreted as days). Default: show all sessions.

CAVEAT on the TOKENS column: it sums each session's prompt_tokens and
completion_tokens, and those are LAST-SNAPSHOT values - they are overwritten
on every turn, while only cost accumulates. So TOKENS reflects each session's
final turn, not everything it consumed. Grouping is also by the session's
CURRENT model, so a session that switched models attributes all of it to
whichever model it ended on. A session with no explicit model selection is
attributed to the model that produced most of its messages.

Pricing: cost is computed with the model's own rates. A model with NO rates
configured has no price, and its zero is then indistinguishable from a free
plan, so its row prints "n/a (no price)" and the TOTAL line counts them as
"+ N unpriced". Tokens are still summed for those rows. To set the rates,
add cost_per_1m_in / cost_per_1m_out / cost_per_1m_in_cached /
cost_per_1m_out_cached (USD per million tokens) to the model under
providers.<provider-id>.models[] in your rush.json, e.g.

  {"providers": {"zai": {"models": [{"id": "glm-4.6",
   "cost_per_1m_in": 0.6, "cost_per_1m_out": 2.2,
   "cost_per_1m_in_cached": 0.11, "cost_per_1m_out_cached": 0.11}]}}}

A model that is genuinely free still has to say so with a zero rate: with no
rates at all this command cannot tell "free" from "nobody priced it".

For accurate per-message token totals, correct per-model attribution and
prompt-cache statistics, use "rush sessions cache" instead - it reads the
per-message usage table rather than these session snapshots. The two are
deliberately NOT merged into one table here: they come from different sources
and presenting them in adjacent columns would imply they are comparable.`,
	Example: `
# Cost grouped by model
rush sessions cost

# Cost grouped by day
rush sessions cost --by day

# Last 7 days, grouped by model
rush sessions cost --since 7d

# Top 20 most expensive sessions
rush sessions cost --by session --top 20

# Set prices for a model that shows n/a (no price), then re-run
rush sessions cost

# Machine-readable output
rush sessions cost --json | jq '.[] | select(.cost_usd > 1.0)'

# Only the rows whose model has no price configured
rush sessions cost --json | jq '.[] | select(.priced == false)'
  `,
	RunE: sessionsCostCmdRun,
}

func sessionsCostCmdRun(cmd *cobra.Command, args []string) error {
	sinceStr, _ := cmd.Flags().GetString("since")
	by, _ := cmd.Flags().GetString("by")
	asJSON, _ := cmd.Flags().GetBool("json")
	topN, _ := cmd.Flags().GetInt("top")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sessions, err := a.Sessions.List(cmd.Context())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Filter out child sessions.
	visible := sessions[:0]
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			continue
		}
		visible = append(visible, s)
	}
	sessions = visible

	// Apply --since filter. #1130: a root's activity is its subtree's — a
	// busy child keeps the root visible even though the root row itself is
	// idle, without the child ever writing the parent's updated_at.
	if sinceStr != "" {
		sinceDur, err := parseSinceDuration(sinceStr)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		cutoff := time.Now().Add(-sinceDur).Unix()
		filtered := sessions[:0]
		for _, s := range sessions {
			active, err := a.Sessions.SubtreeUpdatedAt(cmd.Context(), s.ID)
			if err != nil {
				active = s.UpdatedAt
			}
			if active >= cutoff {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	if len(sessions) == 0 {
		if asJSON {
			fmt.Println("[]")
		} else {
			fmt.Println("(no sessions)")
		}
		return nil
	}

	budgets := sessionBudgets(cmd.Context(), a.Sessions, sessions)

	// The model a session's spend is attributed to and whether its price is
	// configured are read-only display facts, so they are injected rather
	// than reached for through the App inside every grouping: that keeps the
	// grouping functions pure (see costFacts).
	cfg := a.Config()
	msgs := a.Messages
	models := func(s session.Session) costModelRef { return resolveCostModel(cmd.Context(), msgs, s) }
	priced := func(ref costModelRef) bool { return priceKnownFor(cfg, ref) }

	switch by {
	case "model", "":
		return costByModel(sessions, budgets, models, priced, asJSON)
	case "day":
		return costByDay(sessions, budgets, models, priced, asJSON)
	case "session":
		return costBySession(sessions, budgets, models, priced, asJSON, topN)
	case "total":
		return costTotal(sessions, budgets, models, priced, asJSON)
	default:
		return fmt.Errorf("--by: invalid value %q (allowed: model|day|session|total)", by)
	}
}

// parseSinceDuration extends parseDurationDays to also support plain integers
// as days.
func parseSinceDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		return parseDurationDays(s)
	}
	// Try plain integer as days.
	if n, err := parsePlainInt(s); err == nil {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parsePlainInt accepts ONLY a bare integer, e.g. "7".
//
// It used to use fmt.Sscanf(s, "%d", &n), which parses the leading digits and
// reports no error for the trailing remainder. Since parseSinceDuration tries
// this branch before time.ParseDuration, every Go duration was silently
// swallowed as a day count: "2h" became 2 days, "30m" became 30 DAYS, "45s"
// became 45 days, "1h30m" became 1 day. The help text has advertised
// "Go durations (30m, 24h)" the whole time, so --since has been off by orders
// of magnitude for anything but the "7d" and bare-integer forms.
//
// strconv.Atoi rejects trailing characters, which is the whole point here.
func parsePlainInt(s string) (int, error) {
	return strconv.Atoi(s)
}

type costRow struct {
	Key      string  `json:"key"`
	Sessions int     `json:"sessions"`
	Tokens   int64   `json:"tokens"`
	CostUSD  float64 `json:"cost_usd"`
	// Priced is false when no price is configured for the row's model, which
	// is why its cost_usd is 0 and not "free".
	Priced bool `json:"priced"`
}

// costFacts resolves every session's group key, tokens, cost and pricing in
// one pass. models is called AT MOST ONCE PER SESSION, and that is load
// bearing: the fallback path reads the messages table, which is the expensive
// part of this command, and the group key and the Priced flag need the same
// answer -- calling models twice would double that cost for nothing.
//
// key receives the session AND the ref it was resolved from, because the day
// grouping keys off the session's own timestamp and not off its model.
func costFacts(
	sessions []session.Session,
	budgets map[string]float64,
	models func(session.Session) costModelRef,
	priced func(costModelRef) bool,
	key func(session.Session, costModelRef) string,
) []costSessionFact {
	facts := make([]costSessionFact, 0, len(sessions))
	for _, s := range sessions {
		ref := models(s)
		facts = append(facts, costSessionFact{
			Key:    key(s, ref),
			Tokens: s.PromptTokens + s.CompletionTokens,
			Cost:   budgets[s.ID],
			Priced: priced(ref),
		})
	}
	return facts
}

// costModelKey is the group key for --by model: the session's own selection
// when it has one, the dominant producing model otherwise, and
// costModelUnknownKey when neither is known.
func costModelKey(_ session.Session, ref costModelRef) string {
	if !ref.Known() {
		return costModelUnknownKey
	}
	return ref.Model
}

func costByModel(
	sessions []session.Session,
	budgets map[string]float64,
	models func(session.Session) costModelRef,
	priced func(costModelRef) bool,
	asJSON bool,
) error {
	facts := costFacts(sessions, budgets, models, priced, costModelKey)
	rows, total := buildCostGroups(facts, func(a, b costGroup) bool { return a.CostUSD > b.CostUSD })

	if asJSON {
		out := make([]costRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, costRow{Key: r.Key, Sessions: r.Sessions, Tokens: r.Tokens, CostUSD: r.CostUSD, Priced: r.Priced})
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(out)
	}
	return writeCostTable(os.Stdout, "MODEL\tSESSIONS\tTOKENS\tCOST", rows, total)
}

func costByDay(
	sessions []session.Session,
	budgets map[string]float64,
	models func(session.Session) costModelRef,
	priced func(costModelRef) bool,
	asJSON bool,
) error {
	facts := costFacts(sessions, budgets, models, priced, func(s session.Session, _ costModelRef) string {
		return time.Unix(s.UpdatedAt, 0).Format("2006-01-02")
	})
	rows, total := buildCostGroups(facts, func(a, b costGroup) bool { return a.Key < b.Key })

	if asJSON {
		out := make([]costRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, costRow{Key: r.Key, Sessions: r.Sessions, Tokens: r.Tokens, CostUSD: r.CostUSD, Priced: r.Priced})
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(out)
	}
	return writeCostTable(os.Stdout, "DATE\tSESSIONS\tTOKENS\tCOST", rows, total)
}

func costBySession(
	sessions []session.Session,
	budgets map[string]float64,
	models func(session.Session) costModelRef,
	priced func(costModelRef) bool,
	asJSON bool,
	topN int,
) error {
	// No TOTAL row here, so the rows carry the columns the JSON shape needs
	// alongside the shared fact: resolve once per session and sort the
	// result, never the caller's slice.
	type sessionRow struct {
		costSessionFact
		Title     string
		UpdatedAt int64
	}
	rows := make([]sessionRow, 0, len(sessions))
	for _, s := range sessions {
		ref := models(s)
		rows = append(rows, sessionRow{
			costSessionFact: costSessionFact{
				Key:    s.ID,
				Tokens: s.PromptTokens + s.CompletionTokens,
				Cost:   budgets[s.ID],
				Priced: priced(ref),
			},
			Title:     s.Title,
			UpdatedAt: s.UpdatedAt,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Cost > rows[j].Cost })

	if topN > 0 && len(rows) > topN {
		rows = rows[:topN]
	}

	if asJSON {
		type sessionCost struct {
			ID        string  `json:"id"`
			Title     string  `json:"title"`
			Tokens    int64   `json:"tokens"`
			CostUSD   float64 `json:"cost_usd"`
			Priced    bool    `json:"priced"`
			UpdatedAt int64   `json:"updated_at"`
		}
		out := make([]sessionCost, 0, len(rows))
		for _, r := range rows {
			out = append(out, sessionCost{
				ID:        r.Key,
				Title:     r.Title,
				Tokens:    r.Tokens,
				CostUSD:   r.Cost,
				Priced:    r.Priced,
				UpdatedAt: r.UpdatedAt,
			})
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(out)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tTOKENS\tCOST")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			r.Key, truncate(r.Title, 40), formatInt64(r.Tokens), costCellText(r.Cost, r.Priced))
	}
	return tw.Flush()
}

func costTotal(
	sessions []session.Session,
	budgets map[string]float64,
	models func(session.Session) costModelRef,
	priced func(costModelRef) bool,
	asJSON bool,
) error {
	total := costTotals{Sessions: len(sessions)}
	for _, s := range sessions {
		total.Tokens += s.PromptTokens + s.CompletionTokens
		total.CostUSD += budgets[s.ID]
		if !priced(models(s)) {
			total.Unpriced++
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(map[string]any{
			"sessions":          total.Sessions,
			"tokens":            total.Tokens,
			"cost_usd":          total.CostUSD,
			"unpriced_sessions": total.Unpriced,
		})
	}

	fmt.Printf("Sessions:  %d\n", total.Sessions)
	fmt.Printf("Tokens:    %s\n", formatInt64(total.Tokens))
	fmt.Printf("Cost:      $%.3f%s\n", total.CostUSD, costTotalSuffix(total))
	return nil
}

// formatInt64 formats an integer with comma separators.
func formatInt64(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result.WriteByte(',')
		}
		result.WriteRune(c)
	}
	return result.String()
}

func init() {
	sessionsCostCmd.Flags().String("since", "", "Only include sessions updated within this duration (e.g. 7d, 24h, 30m, 3)")
	sessionsCostCmd.Flags().String("by", "model", "Grouping: model|day|session|total")
	sessionsCostCmd.Flags().Bool("json", false, "Emit JSON output")
	sessionsCostCmd.Flags().Int("top", 20, "Number of sessions to show with --by session")
}
