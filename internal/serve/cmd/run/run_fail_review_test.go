package run

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailOnReviewFlagHelpDocumentsCode guards against help/code drift.
// Revert-check: the exit code is documented in --help and must stay in sync
// with reviewFailExitCode.
func TestFailOnReviewFlagHelpDocumentsCode(t *testing.T) {
	t.Parallel()

	flag := runCmd.Flags().Lookup("fail-on-review")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "code 3")
}
