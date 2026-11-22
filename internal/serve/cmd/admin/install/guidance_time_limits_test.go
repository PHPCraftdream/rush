package install

// Moved verbatim from internal/serve/cmd/sessions_help_text_test.go (the one
// installer-only test there); assertions unchanged. The timeLimitsParagraph /
// oneLine helpers are duplicated locally because the originals remain in
// package cmd for the sessions/run help tests.

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/admin/install/targets"

	"github.com/stretchr/testify/require"
)

// oneLine collapses the help text's hard wraps so assertions do not depend on
// where a line happens to break.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// timeLimitsParagraph returns the text from marker (the "Time limits" heading) on.
func timeLimitsParagraph(t *testing.T, s, marker string) string {
	t.Helper()
	i := strings.Index(s, marker)
	require.GreaterOrEqual(t, i, 0, "no Time limits paragraph")
	return oneLine(s[i:])
}

// R8C-6: `rush run` help, README and the embedded guidance said a run has no
// time limit, contradicting the 6h default cap; and that --idle-timeout bounds
// a stuck run, though it only watches turns (a loop waiting on a wedged job is
// bounded by the cap alone).
//
// Revert-check: the old "defaults to 0 (disabled -- no limit)" paragraph turns
// the help assertions red.
func TestRushGuidanceTimeLimits_IdleTimeoutWatchesTurnsOnly(t *testing.T) {
	_, body, err := targets.LoadSkillSource("claude_slash_command", targets.SkillTargetClaude)
	require.NoError(t, err)
	para := timeLimitsParagraph(t, body, "## Time limits")
	require.Contains(t, para, "default six-hour (6 h) TOTAL wall-clock cap")
	require.Contains(t, para, "`RUSH_RUN_DEFAULT_HARD_TIMEOUT` overrides the default cap only")
	require.Contains(t, para, "active work and all waits")
	require.Contains(t, para, "progress does not reset this cap")
	require.Contains(t, para, "If the process remains alive 60 seconds later, the hard backstop force-kills it (exit 124)")
	require.Contains(t, para, "`--idle-timeout` (default `15m`)")
	require.Contains(t, para, "It watches turns only")
	require.Contains(t, para, "do not automatically raise `--timeout` or infer deadlock or its absence from a timeout")
}
