package cmdkit

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseSinceDuration extends parseDurationDays to also support plain integers
// as days.
func ParseSinceDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		return ParseDurationDays(s)
	}
	// Try plain integer as days.
	if n, err := ParsePlainInt(s); err == nil {
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
func ParsePlainInt(s string) (int, error) {
	return strconv.Atoi(s)
}

// parseDurationDays parses a duration string like "7d", "3d", "24h", or
// standard Go duration formats.
func ParseDurationDays(s string) (time.Duration, error) {
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
