// The `rush providers peak-hours` command family: inspect and edit the
// weekly peak-hours policy (timezone, windows, refusal message, pause)
// plus the legacy single-window form, in the --global (default) or
// --local scope.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

// peakWindowRe matches the strict "HH:MM-HH:MM" window token shape used
// by set/add; clock values are validated separately via ParsePeakClock.
var peakWindowRe = regexp.MustCompile(`^(\d{2}:\d{2})-(\d{2}:\d{2})$`)

// providersPeakHoursCmd is the parent of the peak-hours subcommands.
var providersPeakHoursCmd = &cobra.Command{
	Use:   "peak-hours",
	Short: "Inspect and edit a provider's peak-hours refusal policy",
	Long: `Inspect and edit the peak-hours refusal policy of a provider.

Two forms are stored under providers.<id>.peak_hours:

  legacy  {"start","end","message"} — one local-clock HH:MM-HH:MM window
  weekly  {"timezone","windows","message","paused_until"} — the flexible
          form this command family manages; each window has an optional
          days spec (mon..sun, comma lists, ranges, wrap ranges) plus
          start (inclusive) and end (exclusive) HH:MM clocks; 64 max.

Scopes mirror the rest of the providers command tree: --global (the
write default) edits the global rush.json, --local edits ./.rush.
Reads show the merged effective view unless a scope flag is given.

While the policy is in an active window, requests to the provider are
refused with the configured message unless a pause is active. A pause set
with 'rush providers peak-hours pause' temporarily suspends those refusals.`,
	Example: `
rush providers peak-hours list
rush providers peak-hours show zai
rush providers peak-hours set zai mon-fri 14:00-18:00 sat,sun 10:00-12:00
rush providers peak-hours tz zai Asia/Shanghai
rush providers peak-hours pause zai 2h
rush providers peak-hours resume zai
rush providers peak-hours rm zai 2
rush providers peak-hours clear zai
  `,
}

// parsePeakWindowArgs parses the positional "[DAYS] HH:MM-HH:MM ..."
// tokens of set/add into window specs. Days tokens are optional and
// always bind to the window that follows them. Windows are validated
// via config.ParsePeakClock; equal start/end is rejected as a
// zero-length window. Specs carry canonical days text.
func parsePeakWindowArgs(args []string) ([]config.PeakHoursWindowSpec, error) {
	specs := make([]config.PeakHoursWindowSpec, 0, len(args))
	pendingMask, hasPendingDays := [7]bool{}, false
	for i, tok := range args {
		if m := peakWindowRe.FindStringSubmatch(tok); m != nil {
			startMin, err := config.ParsePeakClock(m[1], false)
			if err != nil {
				return nil, fmt.Errorf("window %q: %w", tok, err)
			}
			endMin, err := config.ParsePeakClock(m[2], true)
			if err != nil {
				return nil, fmt.Errorf("window %q: %w", tok, err)
			}
			if startMin == endMin {
				return nil, fmt.Errorf("window %q: start and end are equal — a zero-length window blocks nothing", tok)
			}
			days := ""
			if hasPendingDays {
				days = config.FormatPeakDays(pendingMask)
			}
			specs = append(specs, config.PeakHoursWindowSpec{Days: days, Start: m[1], End: m[2]})
			hasPendingDays = false
			continue
		}
		mask, err := config.ParsePeakDays(tok)
		if err != nil {
			return nil, fmt.Errorf("argument %d (%q): expected a days spec or HH:MM-HH:MM window: %w", i+1, tok, err)
		}
		if hasPendingDays {
			return nil, fmt.Errorf("argument %d (%q): two days specs in a row — each needs its own HH:MM-HH:MM window", i+1, tok)
		}
		if i == len(args)-1 {
			return nil, fmt.Errorf("days spec %q must be followed by an HH:MM-HH:MM window", tok)
		}
		pendingMask, hasPendingDays = mask, true
	}
	return specs, nil
}

// peakWindowKey is the canonical identity of a window: canonical days
// text plus start and end clocks. Used for de-duplication.
func peakWindowKey(spec config.PeakHoursWindowSpec) (string, error) {
	mask, err := config.ParsePeakDays(spec.Days)
	if err != nil {
		return "", err
	}
	return config.FormatPeakDays(mask) + ":" + spec.Start + ":" + spec.End, nil
}

// parsePeakHoursWindow parses a "HH:MM-HH:MM" window string (e.g.
// "09:00-18:00") into a *config.PeakHoursWindow, validating the HH:MM
// values via PeakHoursWindow.Validate. An empty value or the literal
// "off" returns nil (feature off). Returns an error for malformed input.
func parsePeakHoursWindow(raw string) (*config.PeakHoursWindow, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "off") {
		return nil, nil
	}
	parts := strings.SplitN(raw, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid peak-hours %q: expected HH:MM-HH:MM", raw)
	}
	w := &config.PeakHoursWindow{
		Start: strings.TrimSpace(parts[0]),
		End:   strings.TrimSpace(parts[1]),
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// loadTargetEntry returns the raw single-scope peak_hours entry for the
// provider, or nil when the scope has none.
func loadTargetEntry(a *app.App, scope config.Scope, id string) (*config.ProviderPeakHours, error) {
	entry, err := a.Store().PeakHoursEntryForScope(scope, id)
	if err != nil {
		return nil, fmt.Errorf("failed to read peak_hours for provider %q: %w", id, err)
	}
	return entry, nil
}

// peakTargetPolicy composes the policy to edit: the target entry
// converted to the weekly-policy view (a legacy window becomes the
// equivalent whole-week window instead of being dropped), otherwise a
// fresh policy that keeps the effective refusal message so a message
// is not silently dropped when migrating a legacy entry to the weekly
// form.
func peakTargetPolicy(entry *config.ProviderPeakHours, fallbackMessage string) *config.PeakHoursPolicy {
	if entry != nil {
		if pol := entry.AsPolicy(); pol != nil {
			return pol
		}
	}
	return &config.PeakHoursPolicy{Message: fallbackMessage}
}

// peakFieldPath is the sjson path of a provider's peak_hours field.
func peakFieldPath(id string) string {
	return "providers." + escapeSJSONPathComponent(id) + ".peak_hours"
}

// peakScopeArg resolves the --global/--local flags to a write scope.
func peakScopeArg(cmd *cobra.Command) (config.Scope, error) {
	return scopeFromFlags(cmd, config.ScopeGlobal)
}

// peakScopeLabel renders a scope as "global" or "local" (the flag
// names used by the CLI, unlike Scope.String's "workspace").
func peakScopeLabel(scope config.Scope) string {
	if scope == config.ScopeWorkspace {
		return "local"
	}
	return "global"
}

// peakHoursSummary renders the compact PEAK column of providers list
// for either stored form: legacy start-end, or the weekly window count
// with the timezone when one is configured.
func peakHoursSummary(ph *config.ProviderPeakHours) string {
	if ph == nil || (ph.Legacy == nil && ph.Policy == nil) {
		return "—"
	}
	if ph.Legacy != nil {
		peak := ph.Legacy.Start + "-" + ph.Legacy.End
		if ph.Legacy.InPeakHours(time.Now()) {
			peak += " *"
		}
		return peak
	}
	pol := ph.Policy
	peak := fmt.Sprintf("%d windows", len(pol.Windows))
	if pol.Timezone != "" {
		peak += " (" + pol.Timezone + ")"
	}
	if ph.InPeak(time.Now()) {
		peak += " *"
	}
	return peak
}

// peakDisplayLoc resolves the display location for a policy timezone,
// falling back to machine local for unknown names. peakDisplayLabel is
// the human-readable timezone label for output.
func peakDisplayLoc(tzName string) *time.Location {
	if loc, err := config.ResolvePeakTimezone(tzName); err == nil {
		return loc
	}
	return time.Local
}

func peakDisplayLabel(tzName string) string {
	if tzName == "" {
		return "local"
	}
	return tzName
}

// formatPeakTime renders a timestamp in the given location; zero times
// render as an em dash.
func formatPeakTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// peakStatusText is the human one-line status summary used by show.
func peakStatusText(st config.PeakHoursStatus, loc *time.Location) string {
	switch {
	case st.Paused:
		return "paused until " + formatPeakTime(st.PausedUntil, loc)
	case st.InPeak:
		line := "in peak"
		if !st.ReopensAt.IsZero() {
			line += " (reopens " + formatPeakTime(st.ReopensAt, loc) + ")"
		}
		return line
	default:
		return "open"
	}
}

// peakWindowJSON is one window in peak-hours JSON output.
type peakWindowJSON struct {
	Days  string `json:"days,omitempty"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// peakStatusJSON is the evaluated peak-hours state in JSON output.
// RFC3339 strings; zero times are omitted.
type peakStatusJSON struct {
	CheckedAt      string `json:"checked_at"`
	InPeak         bool   `json:"in_peak"`
	Paused         bool   `json:"paused"`
	PausedUntil    string `json:"paused_until,omitempty"`
	Blocked        bool   `json:"blocked"`
	ReopensAt      string `json:"reopens_at,omitempty"`
	NextTransition string `json:"next_transition,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
}

// peakHoursShowJSON is the NDJSON shape of `peak-hours show --json`.
type peakHoursShowJSON struct {
	ID          string           `json:"id"`
	Scope       string           `json:"scope"`
	Timezone    string           `json:"timezone,omitempty"`
	Windows     []peakWindowJSON `json:"windows,omitempty"`
	Message     string           `json:"message,omitempty"`
	PausedUntil string           `json:"paused_until,omitempty"`
	Status      *peakStatusJSON  `json:"status,omitempty"`
}

// peakHoursListJSON is the NDJSON shape of `peak-hours list --json`.
type peakHoursListJSON struct {
	ID          string          `json:"id"`
	Timezone    string          `json:"timezone,omitempty"`
	Windows     int             `json:"windows"`
	PausedUntil string          `json:"paused_until,omitempty"`
	Status      *peakStatusJSON `json:"status"`
}

// rfc3339OrEmpty renders non-zero times as RFC3339, zero as "".
func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// makePeakStatusJSON converts a config status into its JSON shape.
func makePeakStatusJSON(st config.PeakHoursStatus) *peakStatusJSON {
	return &peakStatusJSON{
		CheckedAt:      st.CheckedAt.Format(time.RFC3339),
		InPeak:         st.InPeak,
		Paused:         st.Paused,
		PausedUntil:    rfc3339OrEmpty(st.PausedUntil),
		Blocked:        st.Blocked,
		ReopensAt:      rfc3339OrEmpty(st.ReopensAt),
		NextTransition: rfc3339OrEmpty(st.NextTransition),
		Timezone:       st.Timezone,
	}
}

// makePeakShowJSON builds the show JSON object for an entry; entry
// must be non-nil with a present leg. status is computed at now.
func makePeakShowJSON(id, scopeLabel string, entry *config.ProviderPeakHours, now time.Time) *peakHoursShowJSON {
	out := &peakHoursShowJSON{ID: id, Scope: scopeLabel}
	tzName := ""
	if entry.Legacy != nil {
		out.Windows = append(out.Windows, peakWindowJSON{Start: entry.Legacy.Start, End: entry.Legacy.End})
	} else if entry.Policy != nil {
		tzName = entry.Policy.Timezone
		out.Timezone = tzName
		for _, w := range entry.Policy.Windows {
			out.Windows = append(out.Windows, peakWindowJSON{Days: w.Days, Start: w.Start, End: w.End})
		}
		out.Message = entry.Policy.Message
		out.PausedUntil = entry.Policy.PausedUntil
	}
	out.Status = makePeakStatusJSON(entry.Status(now))
	return out
}

var peakHoursListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List every provider that has a peak-hours policy",
	Long: `Print the merged effective peak-hours view of every provider that
has peak_hours configured (workspace overriding global). STATUS is
computed live. Use --json for one NDJSON object per provider.`,
	Example: `
rush providers peak-hours list --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		providers := a.Config().Providers.Copy()
		ids := make([]string, 0, len(providers))
		for id := range providers {
			if ph := providers[id].PeakHours; ph != nil && (ph.Legacy != nil || ph.Policy != nil) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		now := time.Now()

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, id := range ids {
				ph := providers[id].PeakHours
				item := peakHoursListJSON{
					ID:       id,
					Windows:  peakWindowCount(ph),
					Timezone: peakPolicyTimezone(ph),
					Status:   makePeakStatusJSON(ph.Status(now)),
				}
				if ph.Policy != nil {
					item.PausedUntil = ph.Policy.PausedUntil
				}
				if err := enc.Encode(item); err != nil {
					return err
				}
			}
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTIMEZONE\tWINDOWS\tPAUSED\tSTATUS")
		for _, id := range ids {
			ph := providers[id].PeakHours
			tz := peakDisplayLabel(peakPolicyTimezone(ph))
			windows := fmt.Sprintf("%d", peakWindowCount(ph))
			paused := "—"
			st := ph.Status(now)
			if st.Paused {
				loc := peakDisplayLoc(peakPolicyTimezone(ph))
				paused = formatPeakTime(st.PausedUntil, loc)
			}
			status := "open"
			if st.Blocked {
				status = "blocked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, tz, windows, paused, status)
		}
		return tw.Flush()
	},
}

// peakWindowCount counts stored windows in either form; peakPolicyTimezone
// returns the policy timezone text, if any.
func peakWindowCount(ph *config.ProviderPeakHours) int {
	if ph == nil {
		return 0
	}
	if ph.Legacy != nil {
		return 1
	}
	if ph.Policy != nil {
		return len(ph.Policy.Windows)
	}
	return 0
}

// peakPolicyTimezone returns the policy timezone text, if any.
func peakPolicyTimezone(ph *config.ProviderPeakHours) string {
	if ph != nil && ph.Policy != nil {
		return ph.Policy.Timezone
	}
	return ""
}

var peakHoursShowCmd = &cobra.Command{
	Use:   "show <id>",
	Args:  cobra.ExactArgs(1),
	Short: "Show a provider's effective peak-hours policy",
	Long: `Show the effective (merged) peak-hours policy of a provider: timezone,
numbered windows in stored order (the numbers 'rm' indexes), message,
paused_until, and a live status line. With --global or --local, show
THAT scope's raw stored entry instead. Use --json for JSON.`,
	Example: `
rush providers peak-hours show zai
rush providers peak-hours show zai --local
rush providers peak-hours show zai --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool("json")
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		now := time.Now()

		scoped := cmd.Flags().Changed("global") || cmd.Flags().Changed("local")
		scopeLabel := "effective"
		if scoped {
			scopeLabel = peakScopeLabel(scope)
		}

		var entry *config.ProviderPeakHours
		if scoped {
			entry, err = loadTargetEntry(a, scope, id)
			if err != nil {
				return err
			}
		} else {
			p, ok := a.Config().Providers.Get(id)
			if !ok {
				return fmt.Errorf("provider %q not configured", id)
			}
			entry = p.PeakHours
		}

		if asJSON {
			out := &peakHoursShowJSON{ID: id, Scope: scopeLabel}
			if entry != nil && (entry.Legacy != nil || entry.Policy != nil) {
				out = makePeakShowJSON(id, scopeLabel, entry, now)
			} else {
				out.Status = makePeakStatusJSON((&config.ProviderPeakHours{}).Status(now))
			}
			return json.NewEncoder(os.Stdout).Encode(out)
		}

		if entry == nil || (entry.Legacy == nil && entry.Policy == nil) {
			fmt.Fprintf(os.Stdout, "id:           %s\n", id)
			fmt.Fprintf(os.Stdout, "scope entry (%s); no entry\n", scopeLabel)
			return nil
		}

		if entry.Legacy != nil {
			w := entry.Legacy
			state := peakStatusText(entry.Status(now), time.Local)
			fmt.Fprintf(os.Stdout, "id:           %s\n", id)
			fmt.Fprintf(os.Stdout, "scope:        %s\n", scopeLabel)
			fmt.Fprintf(os.Stdout, "form:         legacy\n")
			fmt.Fprintf(os.Stdout, "windows:\n")
			fmt.Fprintf(os.Stdout, "  1. (every day)  %s-%s\n", w.Start, w.End)
			if w.Message != "" {
				fmt.Fprintf(os.Stdout, "message:      %s\n", w.Message)
			}
			fmt.Fprintf(os.Stdout, "status:       %s (local)\n", state)
			return nil
		}

		pol := entry.Policy
		loc := peakDisplayLoc(pol.Timezone)
		label := peakDisplayLabel(pol.Timezone)
		fmt.Fprintf(os.Stdout, "id:           %s\n", id)
		fmt.Fprintf(os.Stdout, "scope:        %s\n", scopeLabel)
		fmt.Fprintf(os.Stdout, "timezone:     %s\n", label)
		fmt.Fprintf(os.Stdout, "windows:\n")
		for i, w := range pol.Windows {
			days := w.Days
			if days == "" {
				days = "every day"
			}
			fmt.Fprintf(os.Stdout, "  %d. %-12s %s-%s\n", i+1, days, w.Start, w.End)
		}
		if pol.Message != "" {
			fmt.Fprintf(os.Stdout, "message:      %s\n", pol.Message)
		}
		if pol.PausedUntil != "" {
			if until, err := time.Parse(time.RFC3339, pol.PausedUntil); err == nil {
				fmt.Fprintf(os.Stdout, "paused_until: %s (%s)\n", formatPeakTime(until, loc), label)
			} else {
				fmt.Fprintf(os.Stdout, "paused_until: %s\n", pol.PausedUntil)
			}
		}
		st := entry.Status(now)
		fmt.Fprintf(os.Stdout, "status:       %s (%s)\n", peakStatusText(st, loc), label)
		return nil
	},
}

var peakHoursSetCmd = &cobra.Command{
	Use:   "set <id> [DAYS] HH:MM-HH:MM [[DAYS] HH:MM-HH:MM...]",
	Args:  cobra.MinimumNArgs(2),
	Short: "Replace a provider's peak-hours windows",
	Long: `Replace the peak-hours windows of the provider's entry in the chosen
scope. Each window is HH:MM-HH:MM (end exclusive) preceded by an
optional days spec: mon..sun single days, comma lists, ranges
(mon-fri), or wrap ranges (sat-mon). A window without days is active
every day. Up to 64 windows total.

--tz sets the policy timezone (IANA name, UTC offset like UTC+08:00,
utc, or local — the default machine-local clock; storing "local"
clears the timezone). --message sets the refusal text shown while
blocked. Existing paused_until, timezone, and message of the target
entry are preserved unless the corresponding flag is passed.`,
	Example: `
rush providers peak-hours set zai mon-fri 14:00-18:00 sat,sun 10:00-12:00
rush providers peak-hours set zai --local 22:00-06:00
rush providers peak-hours set zai --tz Asia/Shanghai --message "Quota guard" mon-fri 09:00-12:00
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]
		specs, err := parsePeakWindowArgs(args[1:])
		if err != nil {
			return err
		}
		specs = dedupePeakSpecs(specs)
		if len(specs) > config.PeakHoursMaxWindows {
			return fmt.Errorf("too many windows: %d exceeds the cap of %d", len(specs), config.PeakHoursMaxWindows)
		}
		tzName, _ := cmd.Flags().GetString("tz")
		message, _ := cmd.Flags().GetString("message")
		tzChanged := cmd.Flags().Changed("tz")
		messageChanged := cmd.Flags().Changed("message")

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		pol := peakTargetPolicy(entry, effectivePeakMessage(a, id))
		pol.Windows = specs
		if tzChanged {
			if err := applyPeakTimezone(pol, tzName); err != nil {
				return err
			}
		}
		if messageChanged {
			pol.Message = message
		}
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %d peak-hours window(s) for provider %q in %s scope\n", len(pol.Windows), id, scope)
		return nil
	},
}

// dedupePeakSpecs drops identical windows (same canonical days, start,
// and end), keeping the first occurrence.
func dedupePeakSpecs(specs []config.PeakHoursWindowSpec) []config.PeakHoursWindowSpec {
	seen := map[string]bool{}
	out := specs[:0]
	for _, s := range specs {
		key, err := peakWindowKey(s)
		if err != nil || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// effectivePeakMessage returns the merged effective refusal message so
// composing a policy entry from a legacy one keeps the message.
func effectivePeakMessage(a *app.App, id string) string {
	if p, ok := a.Config().Providers.Get(id); ok {
		return p.PeakHours.EffectiveMessage()
	}
	return ""
}

// applyPeakTimezone validates and stores a timezone on the policy; the
// literal "local" (any case) stores "" so the field is omitted.
func applyPeakTimezone(pol *config.PeakHoursPolicy, tzName string) error {
	tzName = strings.TrimSpace(tzName)
	if strings.EqualFold(tzName, "local") {
		pol.Timezone = ""
		return nil
	}
	if _, err := config.ResolvePeakTimezone(tzName); err != nil {
		return err
	}
	pol.Timezone = tzName
	return nil
}

var peakHoursAddCmd = &cobra.Command{
	Use:   "add <id> [DAYS] HH:MM-HH:MM...",
	Args:  cobra.MinimumNArgs(2),
	Short: "Append peak-hours windows to a provider",
	Long: `Append peak-hours windows to the provider's entry in the chosen scope,
creating the entry when the scope has none. Windows already present
(identical days, start, and end) are skipped with a note. Everything
else on the entry is preserved. The total may not exceed 64 windows.`,
	Example: `
rush providers peak-hours add zai sat,sun 10:00-12:00
rush providers peak-hours add zai --local mon-fri 08:00-10:00
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]
		specs, err := parsePeakWindowArgs(args[1:])
		if err != nil {
			return err
		}

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		pol := peakTargetPolicy(entry, effectivePeakMessage(a, id))

		// Append, skipping windows identical to an existing one.
		existing := map[string]bool{}
		for _, s := range pol.Windows {
			if key, err := peakWindowKey(s); err == nil {
				existing[key] = true
			}
		}
		appended := 0
		for _, s := range specs {
			key, err := peakWindowKey(s)
			if err == nil && existing[key] {
				label := s.Days
				if label == "" {
					label = "every day"
				}
				fmt.Fprintf(os.Stderr, "duplicate skipped: %s %s-%s\n", label, s.Start, s.End)
				continue
			}
			if err == nil {
				existing[key] = true
			}
			pol.Windows = append(pol.Windows, s)
			appended++
		}
		if len(pol.Windows) > config.PeakHoursMaxWindows {
			return fmt.Errorf("too many windows: %d would exceed the cap of %d", len(pol.Windows), config.PeakHoursMaxWindows)
		}
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "added %d peak-hours window(s) for provider %q in %s scope\n", appended, id, scope)
		return nil
	},
}

var peakHoursRmCmd = &cobra.Command{
	Use:   "rm <id> <n>...",
	Args:  cobra.MinimumNArgs(2),
	Short: "Remove numbered peak-hours windows from a provider",
	Long: `Remove one or more windows from the provider's entry in the chosen
scope. The numbers are 1-based positions into that scope's stored
window order — the same order 'rush providers peak-hours show <id>
--global|--local' prints. Repeated numbers are rejected.`,
	Example: `
rush providers peak-hours rm zai 2
rush providers peak-hours rm zai 1 3
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		if entry == nil {
			return fmt.Errorf("provider %q has no peak-hours windows in %s scope", id, scope)
		}
		pol := peakTargetPolicy(entry, "")

		// Parse the numbers first so nothing is mutated on a bad input.
		nums := make([]int, 0, len(args)-1)
		seen := map[int]bool{}
		for _, tok := range args[1:] {
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > len(pol.Windows) {
				return fmt.Errorf("invalid window number %q — valid range is 1..%d", tok, len(pol.Windows))
			}
			if seen[n] {
				return fmt.Errorf("duplicate window number %d", n)
			}
			seen[n] = true
			nums = append(nums, n)
		}
		// Delete in descending order so earlier indices stay valid.
		sort.Sort(sort.Reverse(sort.IntSlice(nums)))
		for _, n := range nums {
			pol.Windows = append(pol.Windows[:n-1], pol.Windows[n:]...)
		}
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "removed %d peak-hours window(s) from provider %q in %s scope\n", len(nums), id, scope)
		return nil
	},
}

var peakHoursTzCmd = &cobra.Command{
	Use:   "tz <id> <zone>",
	Args:  cobra.ExactArgs(2),
	Short: "Set the peak-hours timezone of a provider",
	Long: `Set the timezone the provider's peak-hours windows are evaluated in:
an IANA name (Asia/Shanghai), a fixed offset (UTC+08:00, -05:30),
utc, or local (any case) — the machine-local clock, stored as an
omitted timezone. All other fields are preserved.`,
	Example: `
rush providers peak-hours tz zai Asia/Shanghai
rush providers peak-hours tz zai local
rush providers peak-hours tz zai --local utc
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id, tzName := args[0], args[1]

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		pol := peakTargetPolicy(entry, effectivePeakMessage(a, id))
		if err := applyPeakTimezone(pol, tzName); err != nil {
			return err
		}
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "set peak-hours timezone of provider %q to %s in %s scope\n", id, peakDisplayLabel(pol.Timezone), scope)
		return nil
	},
}

var peakHoursClearCmd = &cobra.Command{
	Use:   "clear <id>",
	Args:  cobra.ExactArgs(1),
	Short: "Remove a provider's peak_hours entirely",
	Long: `Remove the providers.<id>.peak_hours field from the chosen scope,
clearing windows, message, timezone, and pause in one step. The
merged effective policy may still come from the other scope.`,
	Example: `
rush providers peak-hours clear zai
rush providers peak-hours clear zai --local
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		if err := a.Store().RemoveConfigField(scope, peakFieldPath(id)); err != nil {
			return fmt.Errorf("failed to clear peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "cleared peak_hours for provider %q in %s scope\n", id, scope)
		return nil
	},
}

var peakHoursPauseCmd = &cobra.Command{
	Use:   "pause <id> [<duration-or-rfc3339>]",
	Args:  cobra.RangeArgs(1, 2),
	Short: "Temporarily suspend a provider's peak-hours refusals",
	Long: `Pause peak-hours refusals for a provider until a point in time: pass
a duration (2h, 90m, 7d, 6d12h) or an RFC3339 timestamp. The pause
may last at most 7 days from now. Without an argument the pause runs
until the end of the current peak window, which requires the provider
to be inside an active peak window right now. While paused, requests
are NOT refused by peak-hours; resume with
'rush providers peak-hours resume'. Passing a duration again replaces an earlier pause.`,
	Example: `
rush providers peak-hours pause zai 2h
rush providers peak-hours pause zai
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]
		now := time.Now()

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		p, ok := a.Config().Providers.Get(id)
		if !ok {
			return fmt.Errorf("provider %q not configured", id)
		}
		st := p.PeakHours.Status(now)
		if st.Paused && len(args) == 1 {
			return fmt.Errorf("provider %q is already paused until %s — resume first or pass a duration",
				id, st.PausedUntil.Format(time.RFC3339))
		}

		var until time.Time
		if len(args) == 2 {
			until, err = config.ParsePeakPauseDuration(args[1], now)
			if err != nil {
				return err
			}
		} else {
			// No argument: pause until the current peak window ends.
			if !st.InPeak || st.ReopensAt.IsZero() {
				return fmt.Errorf("provider %q is not inside an active peak window — pass a duration (e.g. rush providers peak-hours pause %s 2h)", id, id)
			}
			until = st.ReopensAt
		}

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		// Pause writes the effective policy into the chosen scope: when
		// the target scope has no entry, the merged effective policy (a
		// legacy window converted to the weekly form) is copied for consistency.
		pol := peakTargetPolicy(entry, p.PeakHours.EffectiveMessage())
		if entry == nil && p.PeakHours != nil {
			if conv := p.PeakHours.AsPolicy(); conv != nil {
				pol = conv
			}
		}
		pol.PausedUntil = until.Format(time.RFC3339)
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		tz := peakDisplayLabel(peakPolicyTimezone(p.PeakHours))
		fmt.Fprintf(os.Stderr, "paused peak-hours for provider %q until %s (%s) in %s scope\n",
			id, formatPeakTime(until, peakDisplayLoc(peakPolicyTimezone(p.PeakHours))), tz, scope)
		return nil
	},
}

var peakHoursResumeCmd = &cobra.Command{
	Use:   "resume <id>",
	Args:  cobra.ExactArgs(1),
	Short: "End a provider's peak-hours pause in a scope",
	Long: `Clear the paused_until field of the provider's entry in the chosen
scope; an entry that holds nothing else is removed entirely.`,
	Example: `
rush providers peak-hours resume zai
rush providers peak-hours resume zai --local
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		scope, err := peakScopeArg(cmd)
		if err != nil {
			return err
		}
		id := args[0]

		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		entry, err := loadTargetEntry(a, scope, id)
		if err != nil {
			return err
		}
		if entry == nil || entry.Policy == nil || entry.Policy.PausedUntil == "" {
			return fmt.Errorf("provider %q is not paused in %s scope", id, scope)
		}
		pol := peakTargetPolicy(entry, "")
		pol.PausedUntil = ""
		if err := writePeakEntry(a, scope, id, &config.ProviderPeakHours{Policy: pol}); err != nil {
			return fmt.Errorf("failed to write peak_hours: %w", err)
		}
		fmt.Fprintf(os.Stderr, "resumed peak-hours for provider %q in %s scope\n", id, scope)
		return nil
	},
}

func init() {
	// Package-level vars are initialized before any init() runs, so
	// providersCmd exists by the time this registers the subcommands.
	peakHoursListCmd.Flags().Bool("json", false, "Emit one NDJSON object per provider instead of a table")
	peakHoursShowCmd.Flags().Bool("json", false, "Emit a JSON object instead of human-readable lines")

	for _, c := range []*cobra.Command{
		peakHoursListCmd, peakHoursShowCmd, peakHoursSetCmd, peakHoursAddCmd,
		peakHoursRmCmd, peakHoursTzCmd, peakHoursClearCmd, peakHoursPauseCmd,
		peakHoursResumeCmd,
	} {
		c.Flags().Bool("global", false, "Target the global config (~/.local/share/rush/rush.json). Default when neither --global nor --local is given.")
		c.Flags().Bool("local", false, "Target the workspace config (./.rush/rush.json).")
		c.MarkFlagsMutuallyExclusive("global", "local")
	}

	peakHoursSetCmd.Flags().String("tz", "", "Timezone for the windows: IANA name, UTC+08:00 offset, utc, or local (stores an omitted timezone)")
	peakHoursSetCmd.Flags().String("message", "", "Refusal text shown while the provider is blocked. Empty string clears it.")

	providersPeakHoursCmd.AddCommand(
		peakHoursListCmd, peakHoursShowCmd, peakHoursSetCmd, peakHoursAddCmd,
		peakHoursRmCmd, peakHoursTzCmd, peakHoursClearCmd, peakHoursPauseCmd,
		peakHoursResumeCmd,
	)
	providersCmd.AddCommand(providersPeakHoursCmd)
}
