package runfinish

// Fork-only tests for the --fail-on-review exit-code mapping.
import (
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
		code, hit := ReviewVerdictExitCode(tt.verdict, tt.failOnReview)
		assert.Equal(t, tt.wantCode, code, "verdict %q flag %v", tt.verdict, tt.failOnReview)
		assert.Equal(t, tt.wantHit, hit, "verdict %q flag %v", tt.verdict, tt.failOnReview)
	}
}

// TestReviewFailErrorExitCodeAndMessage checks the typed error's public
// contract. Revert-check: the code and the operator-facing message are
// public contract (completion notifications quote the message).
func TestReviewFailErrorExitCodeAndMessage(t *testing.T) {
	t.Parallel()

	e := &ReviewFailError{Verdict: "fail", SessionID: "s9"}
	assert.Equal(t, 3, e.ExitCode())
	msg := e.Error()
	assert.Contains(t, msg, "--fail-on-review")
	assert.Contains(t, msg, `"fail"`)
	assert.Contains(t, msg, "code 3")
	assert.Contains(t, msg, "s9")

	empty := &ReviewFailError{Verdict: "fail"}
	msg = empty.Error()
	assert.Contains(t, msg, "code 3")
	assert.NotContains(t, msg, "<session>")
}
