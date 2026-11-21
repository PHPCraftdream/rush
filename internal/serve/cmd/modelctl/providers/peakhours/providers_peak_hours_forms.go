package provpeak

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

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

		a, err := cmdkit.SetupAppLite(cmd)
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

		a, err := cmdkit.SetupAppLite(cmd)
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

		a, err := cmdkit.SetupAppLite(cmd)
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

		a, err := cmdkit.SetupAppLite(cmd)
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
		fmt.Fprintf(os.Stderr, "set peak-hours timezone of provider %q to %s in %s scope\n", id, PeakDisplayLabel(pol.Timezone), scope)
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

		a, err := cmdkit.SetupAppLite(cmd)
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

		a, err := cmdkit.SetupAppLite(cmd)
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
		tz := PeakDisplayLabel(peakPolicyTimezone(p.PeakHours))
		fmt.Fprintf(os.Stderr, "paused peak-hours for provider %q until %s (%s) in %s scope\n",
			id, formatPeakTime(until, PeakDisplayLoc(peakPolicyTimezone(p.PeakHours))), tz, scope)
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

		a, err := cmdkit.SetupAppLite(cmd)
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
