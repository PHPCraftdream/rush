package cmd

// User-facing strings of `sessions gc` and `sessions why` must say what is
// actually true (review round 1): job retention is NOT a background pass on a
// CLI-only install, and a live delegation is a running async_jobs row on a
// host whose process holds the host lock -- not a "live lock" of a sub-agent
// session.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// oneLine collapses the help text's hard wraps so assertions do not depend on
// where a line happens to break.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Revert-check performed: restored the old "background 60s pass" / "still
// holds a live lock" texts -- both tests FAILED.
func TestSessionsGcHelp_StatesWhatRunsWhere(t *testing.T) {
	long := oneLine(sessionsGcCmd.Long)
	require.NotContains(t, long, "background 60s pass", "there is no background pass on a CLI-only install")
	require.Contains(t, long, "the web server purges every 60s")
	require.Contains(t, long, `each "rush run" loop purges once when it starts`)
	flag := sessionsGcCmd.Flags().Lookup("jobs-older-than")
	require.NotNil(t, flag)
	require.NotContains(t, flag.Usage, "background 7-day pass")
	require.Contains(t, flag.Usage, "web server every 60s")
}

func TestSessionsWhyHelp_NamesHostLockAndSessionLockAccurately(t *testing.T) {
	long := oneLine(sessionsWhyCmd.Long)
	require.NotContains(t, long, "still holds a live lock", "a descendant does not hold a session lock; a delegation row's host does")
	require.NotContains(t, long, "no descendant session is still working", "the verdicts must also count the session's own running job")
	require.Contains(t, long, "host lock")
	require.Contains(t, long, "session lock")
}
