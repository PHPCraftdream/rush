package mailbox

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/stretchr/testify/require"
)

// TestMailbox_PopAllSubmitted verifies that PopAllSubmitted returns all
// queued entries and clears the submitted Queue. This is a helper for
// abandonOwnershipWithHandoff.
func TestMailbox_PopAllSubmitted(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State: MbIdle,
		Submitted: []agentcall.SessionAgentCall{
			{SessionID: "s1", Prompt: "first"},
			{SessionID: "s1", Prompt: "second"},
			{SessionID: "s1", Prompt: "third"},
		},
	}

	all := mb.PopAllSubmitted()
	require.Len(t, all, 3, "must return all three entries")
	require.Equal(t, "first", all[0].Prompt)
	require.Equal(t, "second", all[1].Prompt)
	require.Equal(t, "third", all[2].Prompt)

	// Verify the Queue is cleared.
	require.Len(t, mb.Submitted, 0, "submitted must be cleared")
}

// TestMailbox_PopAllSubmitted_EmptyReturnsNil verifies that PopAllSubmitted
// returns nil (not an empty slice) when the Queue is empty.
func TestMailbox_PopAllSubmitted_EmptyReturnsNil(t *testing.T) {
	t.Parallel()

	mb := &Mailbox{
		State:     MbIdle,
		Submitted: []agentcall.SessionAgentCall{},
	}

	all := mb.PopAllSubmitted()
	require.Nil(t, all, "must return nil for empty Queue")
	require.Len(t, mb.Submitted, 0)
}

// TestMailbox_PopAllSubmitted_DoesNotClearReplacement verifies that
// PopAllSubmitted only clears submitted, not Replacement. The caller
// (AbandonOwnership) folds Replacement into submitted before calling
// PopAllSubmitted, so Replacement should be empty in production use.
func TestMailbox_PopAllSubmitted_DoesNotClearReplacement(t *testing.T) {
	t.Parallel()

	replacementCall := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "Replacement"}
	mb := &Mailbox{
		State:       MbIdle,
		Submitted:   []agentcall.SessionAgentCall{{SessionID: "s1", Prompt: "queued"}},
		Replacement: &replacementCall,
	}

	all := mb.PopAllSubmitted()
	require.Len(t, all, 1, "must return only submitted entries")
	require.Equal(t, "queued", all[0].Prompt)

	// Replacement must still be there.
	require.NotNil(t, mb.Replacement, "Replacement must not be cleared")
	require.Equal(t, "Replacement", mb.Replacement.Prompt)
}
