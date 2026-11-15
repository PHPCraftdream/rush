package cmd

// Fork-only tests for the --fail-on-review exit-code mapping.
import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReviewVerdictExitCode pins the verdict→exit-code selection for
// --fail-on-review. Revert-check: a regression that maps pass→non-zero
// (breaking every green CI run that passes the flag) or fail→0 (breaking
// the feature) is caught here.
func TestReviewVerdictExitCode(t *testing.T) {
	t.Parallel()

	require.Equal(t, 3, reviewFailExitCode)

	tests := []struct {
		verdict      string
		failOnReview bool
		wantCode     int
		wantHit      bool
	}{
		{"fail", true, reviewFailExitCode, true},
		{"fail", false, 0, false},
		{"pass", true, 0, false},
		{"pass_with_notes", true, 0, false},
		{"", true, 0, false},
		{"unverified", true, 0, false},
		{"unparsed", true, 0, false},
		{"error", true, 0, false},
	}
	for _, tt := range tests {
		code, hit := reviewVerdictExitCode(tt.verdict, tt.failOnReview)
		assert.Equal(t, tt.wantCode, code, "verdict %q flag %v", tt.verdict, tt.failOnReview)
		assert.Equal(t, tt.wantHit, hit, "verdict %q flag %v", tt.verdict, tt.failOnReview)
	}
}

// TestReviewFailErrorExitCodeAndMessage checks the typed error's public
// contract. Revert-check: the code and the operator-facing message are
// public contract (completion notifications quote the message).
func TestReviewFailErrorExitCodeAndMessage(t *testing.T) {
	t.Parallel()

	e := &reviewFailError{verdict: "fail", sessionID: "s9"}
	assert.Equal(t, 3, e.ExitCode())
	msg := e.Error()
	assert.Contains(t, msg, "--fail-on-review")
	assert.Contains(t, msg, `"fail"`)
	assert.Contains(t, msg, "code 3")
	assert.Contains(t, msg, "s9")

	empty := &reviewFailError{verdict: "fail"}
	msg = empty.Error()
	assert.Contains(t, msg, "code 3")
	assert.NotContains(t, msg, "<session>")
}

// TestTokensExitCodeHonorsExitCoder verifies the generic exit-coder hook in
// tokensExitCode. Revert-check: only explicitly typed errors get custom
// codes; widening the interface check to plain errors would change every
// subcommand's exit code.
func TestTokensExitCodeHonorsExitCoder(t *testing.T) {
	t.Parallel()

	code, ok := tokensExitCode([]string{"run"}, &reviewFailError{verdict: "fail"})
	assert.Equal(t, 3, code)
	assert.True(t, ok)

	code, ok = tokensExitCode([]string{"run"}, errors.New("boom"))
	assert.Equal(t, 0, code)
	assert.False(t, ok)

	code, ok = tokensExitCode([]string{"run"}, nil)
	assert.Equal(t, 0, code)
	assert.False(t, ok)
}

// TestFailOnReviewFlagHelpDocumentsCode guards against help/code drift.
// Revert-check: the exit code is documented in --help and must stay in sync
// with reviewFailExitCode.
func TestFailOnReviewFlagHelpDocumentsCode(t *testing.T) {
	t.Parallel()

	flag := runCmd.Flags().Lookup("fail-on-review")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "code 3")
}
