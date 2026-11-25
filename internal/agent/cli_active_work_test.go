package agent

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestCLIActiveWorkHeldQuestionPersistence (the ledger held-question job,
// jobOf/armDelegation/heldQuestionJob and the channel-controlled racing
// InsertSessionNotice writer) was DELETED as an obsolete contract: the
// workLedger held-question registry it exercised is gone. The retained
// user-visible contract -- a question-paused delegation keeps the CLI active
// until the child_question notice is durably committed, then releases -- is
// pinned batch-side by TestCLIActiveWorkBatch_MarkerNoticeCommitGate
// (cli_active_work_batch_question_test.go); the ledger-only latches it used
// (answerHold, questionNoticed, heldQuestionJob) have no batch successor.

// Revert-check: removing IsHardQuotaLimit's status guard misclassifies auth errors.
func TestIsHardQuotaLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		text   string
		want   bool
	}{
		{429, "quota exhausted", true},
		{429, "usage limit", true},
		{429, "server overloaded", false},
		{401, "quota exhausted", false},
		{402, "quota exhausted", false},
		{403, "quota exhausted", false},
	} {
		err := fmt.Errorf("wrapped: %w", &fantasy.ProviderError{StatusCode: tc.status, Message: tc.text})
		require.Equal(t, tc.want, clibridge.IsHardQuotaLimit(err))
	}
	require.False(t, clibridge.IsHardQuotaLimit(nil))
	require.False(t, clibridge.IsHardQuotaLimit(fmt.Errorf("quota exhausted HTTP %d", http.StatusTooManyRequests)))
}
