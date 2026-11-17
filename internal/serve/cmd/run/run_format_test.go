package run

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- resolveDefaultHardTimeout -------------------------------------------
//
// Covers the default hard wall-clock backstop installed for `rush run` when
// no --timeout is given (or --timeout 0). The backstop prevents a true zombie
// (deadlock / a read that ignores ctx) from holding its session lock forever.
// These tests exercise the pure resolver rather than waiting hours for a real
// timer to fire.

func TestResolveDefaultHardTimeout_EmptyEnvReturnsDefault(t *testing.T) {
	// No env override → the hardcoded 6h default applies.
	assert.Equal(t, defaultHardKillTimeout, resolveDefaultHardTimeout(""),
		"empty env must yield the default backstop")
	assert.Equal(t, 6*time.Hour, resolveDefaultHardTimeout(""),
		"default backstop must be 6h (backstop-of-last-resort, not task-completion)")
}

func TestResolveDefaultHardTimeout_WhitespaceEnvReturnsDefault(t *testing.T) {
	assert.Equal(t, defaultHardKillTimeout, resolveDefaultHardTimeout("   \t "),
		"whitespace-only env must fall back to default")
}

func TestResolveDefaultHardTimeout_ValidOverrideHonored(t *testing.T) {
	// Valid duration strings (same flexible syntax as --timeout) are honored.
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"900", 900 * time.Second},     // plain number = seconds
		{"30m", 30 * time.Minute},      // Go duration
		{"2h", 2 * time.Hour},          // longer override
		{"90s", 90 * time.Second},      // shorter override
		{"3600", 1 * time.Hour},        // plain seconds for 1h
		{"10000", 10000 * time.Second}, // arbitrary plain number
	}
	for _, c := range cases {
		got := resolveDefaultHardTimeout(c.in)
		assert.Equalf(t, c.want, got, "input %q", c.in)
	}
}

func TestResolveDefaultHardTimeout_InvalidFallsBackToDefault(t *testing.T) {
	// Malformed values must NOT error out a run — they silently fall back.
	invalid := []string{
		"not-a-duration",
		"abc",
		"10x",
		"---",
		"m",
		"h",
	}
	for _, in := range invalid {
		got := resolveDefaultHardTimeout(in)
		assert.Equalf(t, defaultHardKillTimeout, got,
			"invalid input %q must fall back to default, not error", in)
	}
}

func TestResolveDefaultHardTimeout_NonPositiveFallsBackToDefault(t *testing.T) {
	// 0 or negative values are meaningless for a backstop → fall back.
	for _, in := range []string{"0", "-1", "-30m", "-3600"} {
		got := resolveDefaultHardTimeout(in)
		assert.Equalf(t, defaultHardKillTimeout, got,
			"non-positive input %q must fall back to default", in)
	}
}

// --- --timeout / --idle-timeout flag defaults ----------------------------
//
// Regression coverage for the "no overall time limit by default, but a
// bounded inactivity backstop" redesign: `rush run` used to default
// --timeout to 60m; it now defaults to 0 (disabled — unbounded run) and
// relies on the new --idle-timeout (default 15m) to end a genuinely stuck
// invocation. A future edit silently reverting either default would not
// otherwise be caught by any test.

func TestRunCmdTimeoutFlag_DefaultsToDisabled(t *testing.T) {
	flag := runCmd.Flags().Lookup("timeout")
	require.NotNil(t, flag, "--timeout flag must exist")
	assert.Equal(t, "0", flag.DefValue, "rush run must have no overall time limit by default")
}

func TestRunCmdIdleTimeoutFlag_DefaultsTo15Minutes(t *testing.T) {
	flag := runCmd.Flags().Lookup("idle-timeout")
	require.NotNil(t, flag, "--idle-timeout flag must exist")
	assert.Equal(t, "15m", flag.DefValue, "the inactivity backstop must default to 15m")
	dur, err := parseDurationFlexible(flag.DefValue)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, dur)
}

func TestParseDurationFlexible(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"900", 900 * time.Second},
		{"15m", 15 * time.Minute},
		{"90s", 90 * time.Second},
		{"1h", time.Hour},
	}
	for _, c := range cases {
		got, err := parseDurationFlexible(c.in)
		require.NoErrorf(t, err, "input %q", c.in)
		assert.Equalf(t, c.want, got, "input %q", c.in)
	}
}

func TestParseDurationFlexible_Invalid(t *testing.T) {
	_, err := parseDurationFlexible("not-a-duration")
	assert.Error(t, err)
}
