// `rush ps`: text rendering — state derivation, the process table with
// per-purpose sub-lines, and the per-model footer.
package cmd

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

const (
	// psIdleAfterSec is how long an alive process stays "running" after its
	// last request before it displays as idle.
	psIdleAfterSec = 120
	// psStoppedShowWindow is how long a stopped row stays visible by default.
	psStoppedShowWindow = 10 * time.Minute
	// psOtherModelName is the fold target the writer uses when the model
	// list overflows its budget.
	psOtherModelName = "(other)"
)

// psRowState derives the displayed state: running/idle for live processes
// (idle once no request has run for psIdleAfterSec), stopped/stale otherwise.
func psRowState(e heartbeat.Entry, now time.Time) string {
	switch {
	case e.StoredState == "stopped":
		return "stopped"
	case !e.Alive || e.State == "stale":
		return "stale"
	}
	quiet := int64(1 << 62)
	if t := psParseTime(e.LastRequestAt); !t.IsZero() {
		quiet = int64(now.Sub(t).Seconds())
	}
	if e.State == "running" || quiet <= psIdleAfterSec {
		return "running"
	}
	return "idle"
}

// psFrameText renders one full frame: the process table with optional
// per-purpose sub-lines, the per-model footer, and the hidden-row hint.
func psFrameText(rows []psRow, hidden int, slots psSlots, now time.Time) string {
	var b strings.Builder
	if len(rows) == 0 {
		b.WriteString("no active rush processes\n")
		return b.String()
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tAGE\tMODEL\tROLE\tSESSION\tDIR\tPID\tREQS\tTOKENS\tCOST\tSTALE-MODEL")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			r.state,
			psHumanAge(r.beatAgeSec),
			psModelLabel(r),
			psRoleLabel(r.roles),
			psShortSession(r.entry.Session),
			psShortDir(r.entry.LaunchCwd),
			r.entry.PID,
			r.requests,
			psHumanTokens(r.tokens),
			psFormatCost(r.costUSD),
			psStaleLabel(r.stale),
		)
		if len(r.purposes) > 1 {
			for _, p := range r.purposes {
				fmt.Fprintf(tw, "\t\t  %s %dreq %s tok %s\n",
					p.Name, p.Requests, psHumanTokens(p.Tokens), psFormatCost(p.CostUSD))
			}
		}
	}
	tw.Flush()
	b.WriteString("\n")
	b.WriteString(psFooterText(rows, slots, now))
	if hidden > 0 {
		fmt.Fprintf(&b, "%d row(s) hidden (stopped/stale); rerun with --all\n", hidden)
	}
	return b.String()
}

// psFooterText renders the per-model summary over the shown rows plus the
// current slot selection the STALE-MODEL column is judged against.
func psFooterText(rows []psRow, slots psSlots, now time.Time) string {
	var b strings.Builder
	b.WriteString("MODELS (shown rows)\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tLIVE\tREQS\tERR\tLIMITS\tTOKENS\tCOST\tLAST")
	for _, s := range psSummaryRows(rows) {
		last := "-"
		if !s.LastAt.IsZero() {
			last = psHumanAge(max64(0, int64(now.Sub(s.LastAt).Seconds()))) + " ago"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
			s.Model, s.Live, s.Requests, s.Errors, s.LimitHits,
			psHumanTokens(s.Input+s.Output), psFormatCost(s.CostUSD), last)
	}
	tw.Flush()
	b.WriteString(psSlotsLine(slots))
	b.WriteString("\n")
	return b.String()
}

// psSummaryRows aggregates the shown rows per model, ordered most recently
// used first with the model name as a stable tiebreak.
func psSummaryRows(rows []psRow) []heartbeat.ModelSummary {
	entries := make([]heartbeat.Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, r.entry)
	}
	out := heartbeat.Summarize(entries)
	slices.SortStableFunc(out, func(a, b heartbeat.ModelSummary) int {
		if c := b.LastAt.Compare(a.LastAt); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out
}

// psSlotsLine renders the current slot selection, or "unknown" when the
// settings file could not be read (ps must never fail on that).
func psSlotsLine(slots psSlots) string {
	if !slots.known {
		return "SLOTS: unknown"
	}
	parts := make([]string, 0, len(psSlotNames))
	for _, slot := range psSlotNames {
		if m, ok := slots.bySlot[slot]; ok {
			parts = append(parts, slot+"="+psModelKey(m.Provider, m.Model))
		} else {
			parts = append(parts, slot+"=-")
		}
	}
	return "SLOTS: " + strings.Join(parts, " ")
}

// psModelLabel renders the row's most recently used model plus a "+N" when
// several models were used.
func psModelLabel(r psRow) string {
	key := r.modelKey
	if key == psModelKey("", psOtherModelName) {
		key = psOtherModelName
	}
	if key == "" {
		key = "-"
	}
	if r.extraModels > 0 {
		key += fmt.Sprintf(" +%d", r.extraModels)
	}
	return key
}

// psRoleLabel joins up to two roles, collapsing the rest into "+N".
func psRoleLabel(roles []string) string {
	if len(roles) == 0 {
		return "-"
	}
	roles = slices.Clone(roles)
	slices.Sort(roles)
	if len(roles) > 2 {
		return strings.Join(roles[:2], ",") + fmt.Sprintf(" +%d", len(roles)-2)
	}
	return strings.Join(roles, ",")
}

// psStaleLabel renders distinct stale-model causes ("smart:slot@start"),
// collapsing repeats; "-" when the row has none.
func psStaleLabel(stale []psStaleModel) string {
	if len(stale) == 0 {
		return "-"
	}
	labels := make([]string, 0, len(stale))
	seen := map[string]bool{}
	for _, s := range stale {
		l := s.Cause
		if s.Slot != "" {
			l = s.Slot + ":" + s.Cause
		}
		if !seen[l] {
			seen[l] = true
			labels = append(labels, l)
		}
	}
	if len(labels) > 2 {
		labels = append(labels[:2], fmt.Sprintf("+%d", len(labels)-2))
	}
	return strings.Join(labels, ",")
}

// psHumanAge renders seconds as a compact age ("42s", "12m03s", "3h04m", "5d03h").
func psHumanAge(sec int64) string {
	switch {
	case sec < 0:
		sec = 0
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	}
	return fmt.Sprintf("%dd%02dh", sec/86400, (sec%86400)/3600)
}

// psHumanTokens renders token counts compactly ("950", "12.3k", "4.5m").
func psHumanTokens(n int64) string {
	switch {
	case n < 0:
		n = 0
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
}

// psFormatCost renders USD cost with four decimals.
func psFormatCost(c float64) string {
	if c < 0 {
		c = 0
	}
	return fmt.Sprintf("$%.4f", c)
}

// psShortSession truncates a session id to its first 8 runes.
func psShortSession(s string) string {
	if s == "" {
		return "-"
	}
	r := []rune(s)
	if len(r) > 8 {
		return string(r[:8])
	}
	return s
}

// psShortDir renders a launch dir as its base name so no machine-specific
// paths are echoed back.
func psShortDir(dir string) string {
	if dir == "" {
		return "-"
	}
	return filepath.Base(filepath.Clean(dir))
}

// psParseTime parses an RFC3339 snapshot timestamp, returning zero on failure.
func psParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
