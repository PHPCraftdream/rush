package cmd

// User-facing strings of `sessions gc` and `sessions why` must say what is
// actually true (review round 1): job retention is NOT a background pass on a
// CLI-only install, and a live delegation is a running async_jobs row on a
// host whose process holds the host lock -- not a "live lock" of a sub-agent
// session.

import (
	"os"
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
	require.Contains(t, long, `each "rush run" loop purges when it starts and again every 60s while it runs`)
	require.NotContains(t, long, "purges once", "the ticker runs in `rush run` too")
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

// The `sessions` root help lists every subcommand it has: an operator reads it
// to find `jobs`/`why`, and a command missing from it does not exist for them.
//
// Revert-check: the `why <id>`/`jobs <id>` lines removed from the root Long --
// this test went red naming them.
func TestSessionsRootHelp_ListsEverySubcommand(t *testing.T) {
	long := oneLine(sessionsCmd.Long)
	for _, sub := range sessionsCmd.Commands() {
		require.Containsf(t, long, sub.Name(), "`sessions` help does not mention its subcommand %q", sub.Name())
	}
}

// `sessions why`, `gc` and `jobs` say what they read, keep and do (R2C-doc):
// why probes host locks too, gc never purges delivered-but-unreacted debt, and
// the jobs hint stops the WHOLE host process.
//
// Revert-check: the old "only the DB and the .rush/locks directory", the gc
// text without the debt clause and the jobs text without the whole-host
// statement each turn one assertion red.
func TestSessionsHelp_WhyGcJobsAreAccurate(t *testing.T) {
	why := oneLine(sessionsWhyCmd.Long)
	require.NotContains(t, why, "only the DB and the .rush/locks directory")
	require.Contains(t, why, ".rush/hosts", "why probes host locks as well as session locks")

	gc := oneLine(sessionsGcCmd.Long)
	require.Contains(t, gc, "delivered-but-unreacted debt")
	require.Contains(t, gc, "are NEVER purged regardless of age")

	jobs := oneLine(sessionsJobsCmd.Long)
	require.Contains(t, jobs, `"kill -INT <pid>"`)
	require.Contains(t, jobs, "WHOLE host process")
	require.NotContains(t, jobs, "does nothing between turns")
}

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
// the help assertions red; the old README bullet the README ones.
func TestRunHelp_TimeLimitsNamesTheDefaultCap(t *testing.T) {
	para := timeLimitsParagraph(t, runCmd.Long, "Time limits (usually")
	require.Contains(t, para, "6h default wall-clock cap")
	require.Contains(t, para, "RUSH_RUN_DEFAULT_HARD_TIMEOUT")
	require.Contains(t, para, "DURING A TURN", "idle-timeout watches turns only")
	require.Contains(t, para, "bounded only by --timeout or the cap")
	require.NotContains(t, para, "no limit")
	require.NotContains(t, para, "don't need --timeout at all")
}

func TestReadmeTimeouts_NameTheDefaultCap(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	require.NoError(t, err)
	readme := oneLine(string(data))
	i := strings.Index(readme, "**`--timeout <duration>`**")
	require.GreaterOrEqual(t, i, 0)
	bullet := readme[i:]
	if j := strings.Index(bullet, "#### "); j >= 0 {
		bullet = bullet[:j]
	}
	require.Contains(t, bullet, "6 h default wall-clock cap")
	require.Contains(t, bullet, "RUSH_RUN_DEFAULT_HARD_TIMEOUT")
	require.NotContains(t, bullet, "`rush run` otherwise runs until the agent finishes")
	require.Contains(t, readme, "It watches turns only")
}

func TestRushGuidanceTimeLimits_IdleTimeoutWatchesTurnsOnly(t *testing.T) {
	_, body, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	para := timeLimitsParagraph(t, body, "## Time limits")
	require.Contains(t, para, "6 h")
	require.Contains(t, para, "RUSH_RUN_DEFAULT_HARD_TIMEOUT")
	require.Contains(t, para, "It watches turns only")
}

// R8A-2 / R8C-2: the cancel help states the one-shot flag and the --all scope.
func TestSessionsCancelHelp_DescribesOneShotFlagAndAllScope(t *testing.T) {
	long := oneLine(sessionsCancelCmd.Long)
	require.Contains(t, long, "ONE-SHOT")
	require.Contains(t, long, "cleared when it is honoured")
	require.Contains(t, long, "With --all only sessions that have live work are flagged")
	require.Contains(t, oneLine(sessionsCancelCmd.Flags().Lookup("all").Usage), "live work")
}
