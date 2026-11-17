package run

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseDurationFlexible parses a duration string that can be a plain integer
// (interpreted as seconds) or a Go duration string (e.g. "30s", "5m", "1h").
func parseDurationFlexible(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

// defaultHardKillTimeout is the default wall-clock cap of `rush run` when the
// operator does NOT pass --timeout (or passes --timeout 0): a graceful deadline
// like --timeout (installRunDeadline), whose hard-kill grace is the zombie
// backstop. 6h is long enough for ordinary work and short enough that a wedged
// process does not hold its session lock across days; a longer wait (a soak
// test) needs an explicit --timeout.
const defaultHardKillTimeout = 6 * time.Hour

// idleTimeoutDisabledSentinel stands in for "no inactivity backstop" when
// the operator passes --idle-timeout 0: the underlying stream watchdog has
// no native off-switch, only a threshold, so a duration long enough to
// never practically fire (well short of overflowing time.Time arithmetic)
// serves the same purpose. The run is still eventually bounded by
// --timeout (if set) and the default wall-clock cap.
const idleTimeoutDisabledSentinel = 100 * 365 * 24 * time.Hour

// resolveDefaultHardTimeout resolves the default hard-kill backstop duration
// from the RUSH_RUN_DEFAULT_HARD_TIMEOUT env var, falling back to
// defaultHardKillTimeout when the env var is unset, empty, or holds an
// unparseable / non-positive value. It NEVER returns an error: a malformed env
// var must not error out a run, it just silently falls back to the hardcoded
// default. Extracted as a pure function so it is directly unit-testable.
func resolveDefaultHardTimeout(envVal string) time.Duration {
	envVal = strings.TrimSpace(envVal)
	if envVal == "" {
		return defaultHardKillTimeout
	}
	d, err := parseDurationFlexible(envVal)
	if err != nil || d <= 0 {
		return defaultHardKillTimeout
	}
	return d
}

// parseTokenCount parses a token count string with optional k/M suffix.
// e.g. "500000", "100k", "1M" → int64.
func parseTokenCount(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1_000_000
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k"):
		multiplier = 1_000
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid token count %q", s)
	}
	return n * multiplier, nil
}
