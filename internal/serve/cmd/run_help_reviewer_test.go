package cmd

// `rush run --help` is the operator's only in-terminal documentation of the
// reviewer pass, and it is prose: no compiler, schema or wire check keeps it
// true. This test pins the contract both an operator and a wrapper reading
// `--json` depend on — the envelope carries the parsed verdict
// `review_verdict` beside `review`, the reviewer turn holds a read-only
// toolset, and the verdict is an opinion that leaves the exit code
// not changed by it.
//
// A regression here is not a typo: it is the help going back to describing
// the reviewer as a slot with a full write+shell toolset whose verdict
// silently decided a run's fate — exactly what the reviewer-pass verification
// work (#1165) removed. Revert-check: restoring the old paragraph (only
// "review", no read-only toolset, no unverified/unparsed/error states, no
// "the exit code is not changed by it") makes every assertion below fail.
//
// Note the phrasing: the fact must be carried by a phrase short enough to
// survive cobra's wrapping on an 80-column terminal. "does not change the
// exit code" is 29 characters and the wire split it across two lines, so the
// substring never appeared contiguously and the collapsed oneLine() assertion
// could not catch it. The wording below keeps "not changed by it" (17
// characters) together on a single wire line.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunHelp_DocumentsReviewerVerdictAndReadOnly(t *testing.T) {
	long := oneLine(runCmd.Long)
	require.Contains(t, long, "review_verdict")
	require.Contains(t, long, "read-only")
	require.Contains(t, long, "unverified")
	require.Contains(t, long, "unparsed")
	require.Contains(t, long, "pass_with_notes")
	require.Contains(t, long, "not changed by it")
}

// TestRunHelp_ExitCodePhraseSurvivesWrapping pins the help text the way an
// operator actually sees it: cobra (and then the terminal) re-wrap Long to the
// terminal width, so a phrase that straddles a source line break can be split
// again on the wire even though oneLine(L) — which collapses every newline —
// sees it whole. This therefore asserts on the RAW text at the line break,
// keeping the verdict-does-not-affect-exit-code fact on one wire line.
func TestRunHelp_ExitCodePhraseSurvivesWrapping(t *testing.T) {
	require.Contains(t, runCmd.Long, "the exit code is\n                      not changed by it",
		"the sentence must be wrapped so the phrase stays contiguous on the wire; cobra re-wraps Long to the terminal width, so a phrase split across two source lines can be split again on the wire")
}
